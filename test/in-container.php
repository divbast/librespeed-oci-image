<?php

// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

declare(strict_types=1);

/*
 * There is no shell in this image. It also has no dependencies.
 *
 * Output protocol: one tab-separated record per assertion,
 *
 *     ok<TAB>what<TAB>expected<TAB>found
 *     FAIL<TAB>what<TAB>expected<TAB>found
 *
 * php-ini and probe-getip are the exceptions: they print what they read and
 * assert nothing.
 */

/**
 * The ones most likely to be the target of an intrusion.
 */
const FORBIDDEN_SHELLS = ['/bin/sh', '/bin/bash', '/bin/dash'];

const NO_CAPABILITIES = '0000000000000000';
const EGRESS_PROBE_HOST = '9.9.9.9';
const EGRESS_PROBE_PORT = 443;
const EGRESS_PROBE_TIMEOUT = 3;
const ROOT_WRITE_PROBE = '/.librespeed-write-probe';
const READ_ONLY_ERROR = 'Read-only file system';
const LOOPBACK_HOST = '127.0.0.1';
const LOOPBACK_PORT = 8080;
const LOOPBACK_TIMEOUT = 5;

/**
 * Collects assertions and renders them in the record format above.
 */
final class Assertions
{
    private static int $failures = 0;

    public static function equals(string $what, string $expected, string $found): void
    {
        self::record($found === $expected, $what, $expected, $found);
    }

    public static function record(bool $ok, string $what, string $expected, string $found): void
    {
        fwrite(STDOUT, sprintf(
            "%s\t%s\t%s\t%s\n",
            $ok ? 'ok' : 'FAIL',
            self::oneLine($what),
            self::oneLine($expected),
            self::oneLine($found),
        ));

        if (!$ok) {
            ++self::$failures;
        }
    }

    public static function failures(): int
    {
        return self::$failures;
    }

    /** Tabs and newlines are the record separators so they can't be present in any field. */
    private static function oneLine(string $text): string
    {
        return trim(strtr($text, ["\t" => ' ', "\n" => ' ', "\r" => ' ']));
    }
}

function readOrThrow(string $path): string
{
    $contents = @file_get_contents($path);
    if ($contents === false) {
        throw new RuntimeException("cannot read $path");
    }

    return $contents;
}

// --- subcommands ------------------------------------------------------------

function assertPid1(): void
{
    Assertions::equals('PID 1 comm', 'frankenphp', trim(readOrThrow('/proc/1/comm')));
}

/**
 * Extract capability mask from /proc/self/status as a value.
 */
function capabilityMask(string $status, string $field): string
{
    $pattern = '/^' . preg_quote($field, '/') . ':[ \t]*([0-9a-fA-F]+)[ \t]*$/m';

    if (preg_match($pattern, $status, $matches) !== 1) {
        throw new RuntimeException("/proc/self/status has no well-formed $field line");
    }

    return $matches[1];
}

function assertCaps(): void
{
    $status = readOrThrow('/proc/self/status');

    foreach (['CapEff', 'CapPrm'] as $field) {
        $mask = capabilityMask($status, $field);
        Assertions::record(
            hexdec($mask) === 0,
            "$field, every capability bit clear",
            NO_CAPABILITIES,
            $mask,
        );
    }
}

function assertNoShell(): void
{
    foreach (FORBIDDEN_SHELLS as $path) {
        clearstatcache(true, $path);
        $present = file_exists($path) || is_link($path);

        Assertions::record(
            !$present,
            "$path is absent from the image",
            'absent',
            $present ? 'present' : 'absent',
        );
    }
}

/**
 * The kernel answers a create on a read-only mount with EROFS before it
 * checks permissions on the directory, so the error names the mount. A root
 * that was merely owned by root would answer "Permission denied" instead,
 * and this would fail.
 */
function assertReadOnly(): void
{
    error_clear_last();
    $written = @file_put_contents(ROOT_WRITE_PROBE, "probe\n");
    $error = error_get_last()['message'] ?? 'no error';

    if ($written !== false) {
        @unlink(ROOT_WRITE_PROBE);
    }

    Assertions::record(
        $written === false && str_contains($error, READ_ONLY_ERROR),
        'writing ' . ROOT_WRITE_PROBE . ' fails because / is read-only',
        READ_ONLY_ERROR,
        $written === false ? $error : "wrote $written bytes",
    );
}

/**
 * Measures from inside the container.
 */
function probeEgress(): void
{
    $errno = 0;
    $errstr = '';
    $socket = @fsockopen(
        EGRESS_PROBE_HOST,
        EGRESS_PROBE_PORT,
        $errno,
        $errstr,
        EGRESS_PROBE_TIMEOUT,
    );

    $connected = $socket !== false;
    if ($socket !== false) {
        fclose($socket);
    }

    Assertions::record(
        !$connected,
        'outbound TCP to ' . EGRESS_PROBE_HOST . ':' . EGRESS_PROBE_PORT . ' is refused',
        'no connection',
        $connected ? 'CONNECTED' : sprintf('no connection (errno %d: %s)', $errno, $errstr),
    );
}

/**
 * How PHP itself reads a boolean directive: on, yes and true, or else the
 * number the value starts with. display_errors also takes stderr and stdout,
 * and both mean on.
 */
function iniEnabled(string $directive): bool
{
    $value = strtolower(trim((string) ini_get($directive)));

    return in_array($value, ['on', 'yes', 'true', 'stderr', 'stdout'], true) || (int) $value !== 0;
}

function phpIni(): void
{
    $postMaxSize = (string) ini_get('post_max_size');

    fwrite(STDOUT, json_encode([
        'allow_url_fopen' => iniEnabled('allow_url_fopen'),
        'display_errors' => iniEnabled('display_errors'),
        'post_max_size' => $postMaxSize,
        // In bytes, as PHP reads the K, M and G suffixes
        'post_max_size_bytes' => ini_parse_quantity($postMaxSize),
    ], JSON_THROW_ON_ERROR) . "\n");
}

/**
 * Prints the body of GET /backend/getIP.php as the server answers it over
 * loopback. allow_url_fopen is off for this SAPI too, so it speaks HTTP over
 * a bare socket. HTTP/1.0 keeps the answer unchunked, ending where the
 * server closes the connection.
 */
function probeGetIp(): void
{
    $errno = 0;
    $errstr = '';
    $socket = @fsockopen(LOOPBACK_HOST, LOOPBACK_PORT, $errno, $errstr, LOOPBACK_TIMEOUT);
    if ($socket === false) {
        throw new RuntimeException(sprintf(
            'cannot connect to %s:%d (errno %d: %s)',
            LOOPBACK_HOST,
            LOOPBACK_PORT,
            $errno,
            $errstr,
        ));
    }

    stream_set_timeout($socket, LOOPBACK_TIMEOUT);
    $request = "GET /backend/getIP.php HTTP/1.0\r\nHost: " . LOOPBACK_HOST . ':' . LOOPBACK_PORT . "\r\n\r\n";
    $sent = fwrite($socket, $request);
    $response = stream_get_contents($socket);
    fclose($socket);

    if ($sent !== strlen($request) || $response === false) {
        throw new RuntimeException('the exchange with ' . LOOPBACK_HOST . ':' . LOOPBACK_PORT . ' broke off');
    }

    $parts = explode("\r\n\r\n", $response, 2);
    $statusLine = strtok($parts[0], "\r\n");
    if (!isset($parts[1]) || $statusLine === false || preg_match('#^HTTP/\d\.\d 200 #', $statusLine) !== 1) {
        throw new RuntimeException('expected a 200 response, got: ' . ($statusLine === false ? 'nothing' : $statusLine));
    }

    fwrite(STDOUT, $parts[1]);
}

// --- dispatch ---------------------------------------------------------------

/**
 * @return array<string, array{arguments: list<string>, handler: Closure}>
 */
function commands(): array
{
    return [
        'assert-pid1' => ['arguments' => [], 'handler' => assertPid1(...)],
        'assert-caps' => ['arguments' => [], 'handler' => assertCaps(...)],
        'assert-no-shell' => ['arguments' => [], 'handler' => assertNoShell(...)],
        'assert-read-only' => ['arguments' => [], 'handler' => assertReadOnly(...)],
        'probe-egress' => ['arguments' => [], 'handler' => probeEgress(...)],
        'php-ini' => ['arguments' => [], 'handler' => phpIni(...)],
        'probe-getip' => ['arguments' => [], 'handler' => probeGetIp(...)],
    ];
}

/**
 * @param array<string, array{arguments: list<string>, handler: Closure}> $commands
 */
function usage(array $commands): string
{
    $lines = ['usage: php-cli in-container.php <subcommand> [arguments]', '', 'subcommands:'];

    foreach ($commands as $name => $command) {
        $placeholders = array_map(
            static fn (string $argument): string => '<' . $argument . '>',
            $command['arguments'],
        );
        $lines[] = '  ' . rtrim($name . ' ' . implode(' ', $placeholders));
    }

    return implode("\n", $lines) . "\n";
}

/**
 * @param list<string> $arguments
 */
function main(array $arguments): int
{
    $commands = commands();
    $name = $arguments[0] ?? '';

    if (!isset($commands[$name])) {
        fwrite(STDERR, $name === '' ? "no subcommand given\n" : "unknown subcommand '$name'\n");
        fwrite(STDERR, usage($commands));

        return 2;
    }

    $command = $commands[$name];
    $operands = array_slice($arguments, 1);
    $expected = count($command['arguments']);

    if (count($operands) !== $expected) {
        fwrite(STDERR, sprintf(
            "%s takes %d argument(s) (%s), got %d\n",
            $name,
            $expected,
            $expected === 0 ? 'none' : implode(', ', $command['arguments']),
            count($operands),
        ));

        return 2;
    }

    try {
        ($command['handler'])(...$operands);
    } catch (Throwable $problem) {
        Assertions::record(
            false,
            "$name completed",
            'no exception',
            $problem::class . ': ' . $problem->getMessage(),
        );
    }

    return Assertions::failures() === 0 ? 0 : 1;
}

/**
 * $argv exists only when register_argc_argv is on.
 *
 * The subcommand and operands are whatever follows this script's own path.
 * The native CLI puts that path at $argv[0], but FrankenPHP 1.13.0's php-cli
 * put its own binary there and shifted the script to $argv[1]
 * (php/frankenphp#2690, fixed in 1.13.1). Finding the script by identity
 * rather than by position reads both.
 *
 * @return list<string>
 */
function commandLine(): array
{
    $raw = $_SERVER['argv'] ?? null;
    if (!is_array($raw)) {
        return [];
    }

    $arguments = [];
    foreach ($raw as $value) {
        if (is_string($value)) {
            $arguments[] = $value;
        }
    }

    foreach ($arguments as $index => $argument) {
        if (realpath($argument) === __FILE__) {
            return array_slice($arguments, $index + 1);
        }
    }

    fwrite(STDERR, sprintf(
        "%s is not in argv, so there is no telling where the subcommand starts: %s\n",
        __FILE__,
        implode(' ', $arguments),
    ));

    return [];
}

exit(main(commandLine()));

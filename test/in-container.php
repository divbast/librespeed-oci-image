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
 * php-ini, probe-getip, read-salt, read-row, http-request,
 * http-request-locked, lock-holders and hold-db-lock are the exceptions:
 * they print what they read or do, and assert nothing.
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
const TELEMETRY_DB = '/data/telemetry.db';
const SALT_FILE = '/data/idObfuscation_salt.php';
// The first bytes of every SQLite database file
const SQLITE_MAGIC = "SQLite format 3\0";
// Where login.php records failed logins, and the lock it holds while it
// checks a password
const STATS_LOGIN_DB = '/data/stats-login.db';
const STATS_LOGIN_LOCK = '/tmp/librespeed/stats-login.lock';
// What login.php checks a password against, read on every request
const STATS_PASSWORD_HASH = '/data/stats-password.hash';
// Every lock the kernel holds, one per line, as fcntl(2) and flock(2) take them
const PROC_LOCKS = '/proc/locks';
const HTTP_TIMEOUT = 10;
// An HTTP field name is a token: RFC 9110, section 5.6.2
const HEADER_LINE = '/\A[!#$%&\'*+.^_`|~0-9A-Za-z-]+:[^\r\n]*\z/';
// Every TCP socket in the container's network namespace, one per line after
// a header
const TCP_TABLES = ['/proc/net/tcp', '/proc/net/tcp6'];
// sl, local address:port and remote address:port in hex, the state, which is
// 0A for a listening socket, then five columns, then the socket's inode
const TCP_SOCKET = '/\A\s*\d+:\s+[0-9A-Fa-f]+:([0-9A-Fa-f]{4})\s+[0-9A-Fa-f]+:[0-9A-Fa-f]{4}\s+([0-9A-Fa-f]{2})(?:\s+\S+){5}\s+(\d+)\s/';
const TCP_LISTEN = '0A';
// The speed test and the health endpoint, and nothing else
const CONTRACT_PORTS = '8080 8081';

/**
 * Every error level PHP 8.5 still raises, by bit. E_STRICT is gone: its
 * constant is deprecated and no error carries it.
 */
const ERROR_LEVELS = [
    'E_ERROR' => E_ERROR,
    'E_WARNING' => E_WARNING,
    'E_PARSE' => E_PARSE,
    'E_NOTICE' => E_NOTICE,
    'E_CORE_ERROR' => E_CORE_ERROR,
    'E_CORE_WARNING' => E_CORE_WARNING,
    'E_COMPILE_ERROR' => E_COMPILE_ERROR,
    'E_COMPILE_WARNING' => E_COMPILE_WARNING,
    'E_USER_ERROR' => E_USER_ERROR,
    'E_USER_WARNING' => E_USER_WARNING,
    'E_USER_NOTICE' => E_USER_NOTICE,
    'E_RECOVERABLE_ERROR' => E_RECOVERABLE_ERROR,
    'E_DEPRECATED' => E_DEPRECATED,
    'E_USER_DEPRECATED' => E_USER_DEPRECATED,
];

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
 * The inode of every socket a process in the container holds open
 *
 * @return array<int, true>
 */
function heldSockets(): array
{
    $held = [];
    foreach (glob('/proc/[0-9]*/fd/*') ?: [] as $fd) {
        $target = @readlink($fd);
        if (is_string($target) && preg_match('/\Asocket:\[(\d+)\]\z/', $target, $socket) === 1) {
            $held[(int) $socket[1]] = true;
        }
    }

    return $held;
}

/**
 * Every port a process in the container listens on, from the kernel's own
 * tables, so a listener no process advertises is counted too. Only sockets
 * a process here holds count: Docker's resolver for a user-defined network
 * listens in the same namespace, at 127.0.0.11 on a port of its own, but
 * from outside the container. If no descriptor could be read, nothing
 * counts, and the two ports the contract names are missing.
 */
function assertListeners(): void
{
    $held = heldSockets();
    $ports = [];
    foreach (TCP_TABLES as $table) {
        foreach (array_slice(explode("\n", trim(readOrThrow($table))), 1) as $line) {
            if (preg_match(TCP_SOCKET, $line, $socket) !== 1) {
                throw new RuntimeException("$table has a line that is not a socket: $line");
            }
            if (strtoupper($socket[2]) === TCP_LISTEN && isset($held[(int) $socket[3]])) {
                $ports[] = (int) hexdec($socket[1]);
            }
        }
    }
    $ports = array_unique($ports);
    sort($ports);

    Assertions::equals('TCP ports a process in the container listens on', CONTRACT_PORTS, implode(' ', $ports));
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

/**
 * The levels a mask holds, by name, with any bit no level names left as a
 * number
 *
 * @return list<string>
 */
function errorLevels(int $mask): array
{
    $levels = [];
    foreach (ERROR_LEVELS as $name => $bit) {
        if (($mask & $bit) !== 0) {
            $levels[] = $name;
            $mask &= ~$bit;
        }
    }
    if ($mask !== 0) {
        $levels[] = (string) $mask;
    }

    return $levels;
}

function phpIni(): void
{
    $postMaxSize = (string) ini_get('post_max_size');

    fwrite(STDOUT, json_encode([
        'allow_url_fopen' => iniEnabled('allow_url_fopen'),
        'display_errors' => iniEnabled('display_errors'),
        'log_errors' => iniEnabled('log_errors'),
        'error_log' => (string) ini_get('error_log'),
        // error_reporting() is the level in force, E_ALL when nothing set it
        'error_reporting' => errorLevels(error_reporting()),
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

/**
 * Overwrites the database's header, so that SQLite no longer recognises the
 * file, as a disk fault or a half-restored backup would leave it.
 */
function damageDb(): void
{
    $handle = @fopen(TELEMETRY_DB, 'r+b');
    if ($handle === false) {
        throw new RuntimeException('cannot open ' . TELEMETRY_DB . ' to damage it');
    }
    $written = fwrite($handle, str_repeat('x', strlen(SQLITE_MAGIC)));
    fclose($handle);

    clearstatcache(true, TELEMETRY_DB);
    $header = (string) @file_get_contents(TELEMETRY_DB, false, null, 0, strlen(SQLITE_MAGIC));
    Assertions::record(
        $written === strlen(SQLITE_MAGIC) && $header !== SQLITE_MAGIC,
        TELEMETRY_DB . ' no longer starts with the SQLite header',
        'damaged',
        $header === SQLITE_MAGIC ? 'intact' : 'damaged',
    );
}

/**
 * Removes the database and whatever journal SQLite left beside it.
 */
function removeDb(): void
{
    foreach ([TELEMETRY_DB, TELEMETRY_DB . '-journal', TELEMETRY_DB . '-wal', TELEMETRY_DB . '-shm'] as $path) {
        if (file_exists($path)) {
            @unlink($path);
        }
    }

    clearstatcache(true, TELEMETRY_DB);
    Assertions::record(
        !file_exists(TELEMETRY_DB),
        TELEMETRY_DB . ' is gone',
        'absent',
        file_exists(TELEMETRY_DB) ? 'present' : 'absent',
    );
}

/**
 * Prints the salt file's contents and its mode, as JSON.
 */
function readSalt(): void
{
    $contents = readOrThrow(SALT_FILE);
    $perms = @fileperms(SALT_FILE);
    if ($perms === false) {
        throw new RuntimeException('cannot stat ' . SALT_FILE);
    }

    fwrite(STDOUT, json_encode([
        'contents' => $contents,
        'mode' => sprintf('%04o', $perms & 0o7777),
    ], JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES) . "\n");
}

/**
 * Prints one stored result, by the database's own id, as JSON. It opens the
 * database read-only, so it never creates one that is missing.
 */
function readRow(string $id): void
{
    if (preg_match('/^[1-9][0-9]*$/', $id) !== 1) {
        throw new InvalidArgumentException("'$id' is not a row id");
    }
    if (!is_file(TELEMETRY_DB)) {
        throw new RuntimeException(TELEMETRY_DB . ' does not exist');
    }

    $pdo = new PDO('sqlite:' . TELEMETRY_DB, null, null, [
        PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
        Pdo\Sqlite::ATTR_OPEN_FLAGS => Pdo\Sqlite::OPEN_READONLY,
    ]);
    $statement = $pdo->prepare(
        'SELECT id, ip, ispinfo, extra, ua, lang, dl, ul, ping, jitter, log FROM speedtest_users WHERE id = ?',
    );
    $statement->execute([(int) $id]);
    $row = $statement->fetch(PDO::FETCH_ASSOC);
    if (!is_array($row)) {
        throw new RuntimeException('no row ' . $id . ' in ' . TELEMETRY_DB);
    }

    fwrite(STDOUT, json_encode($row, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE) . "\n");
}

/**
 * Moves every failed login login.php has recorded the given number of
 * seconds into the past, so its window can be tested without waiting it
 * out. Moving none fails: a test that ages failures expects some.
 */
function ageFailures(string $seconds): void
{
    if (preg_match('/^[1-9][0-9]*$/', $seconds) !== 1) {
        throw new InvalidArgumentException("'$seconds' is not a number of seconds");
    }
    if (!is_file(STATS_LOGIN_DB)) {
        throw new RuntimeException(STATS_LOGIN_DB . ' does not exist');
    }

    $pdo = new PDO('sqlite:' . STATS_LOGIN_DB, null, null, [
        PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
        PDO::ATTR_TIMEOUT => HTTP_TIMEOUT,
    ]);
    $statement = $pdo->prepare('UPDATE failures SET at = at - ?');
    $statement->execute([(int) $seconds]);
    $moved = $statement->rowCount();

    Assertions::record(
        $moved > 0,
        "the recorded login failures, moved $seconds s into the past",
        'at least one',
        (string) $moved,
    );
}

/**
 * Sends one request over a bare socket, as a proxy in front of the server
 * would, and prints the status code on the first line and the body after
 * it. headers holds one "Name: value" per line. A body goes form-encoded,
 * as upstream's login form posts it. HTTP/1.0 keeps the answer unchunked,
 * ending where the server closes the connection.
 */
function httpRequest(string $method, string $url, string $headers, string $body): void
{
    if (preg_match('/\A[A-Z]+\z/', $method) !== 1) {
        throw new InvalidArgumentException("'$method' is not a method");
    }
    $parts = parse_url($url);
    if ($parts === false || ($parts['scheme'] ?? '') !== 'http' || !isset($parts['host'])) {
        throw new InvalidArgumentException("'$url' is not an http:// URL");
    }
    $host = $parts['host'];
    $port = $parts['port'] ?? 80;
    $target = ($parts['path'] ?? '/') . (isset($parts['query']) ? '?' . $parts['query'] : '');

    $lines = ["$method $target HTTP/1.0", "Host: $host:$port", 'Connection: close'];
    foreach (explode("\n", $headers) as $header) {
        if ($header === '') {
            continue;
        }
        if (preg_match(HEADER_LINE, $header) !== 1) {
            throw new InvalidArgumentException("'$header' is not a header line");
        }
        $lines[] = $header;
    }
    if ($body !== '') {
        $lines[] = 'Content-Type: application/x-www-form-urlencoded';
    }
    if ($body !== '' || $method === 'POST') {
        $lines[] = 'Content-Length: ' . strlen($body);
    }
    $request = implode("\r\n", $lines) . "\r\n\r\n" . $body;

    $errno = 0;
    $errstr = '';
    $socket = @fsockopen($host, $port, $errno, $errstr, HTTP_TIMEOUT);
    if ($socket === false) {
        throw new RuntimeException(sprintf('cannot connect to %s:%d (errno %d: %s)', $host, $port, $errno, $errstr));
    }
    stream_set_timeout($socket, HTTP_TIMEOUT);
    $sent = fwrite($socket, $request);
    $response = stream_get_contents($socket);
    fclose($socket);

    if ($sent !== strlen($request) || $response === false) {
        throw new RuntimeException("the exchange with $host:$port broke off");
    }

    $split = explode("\r\n\r\n", $response, 2);
    if (preg_match('#\AHTTP/\d\.\d (\d{3})#', $split[0], $status) !== 1) {
        throw new RuntimeException('no HTTP status line in the answer: ' . substr($split[0], 0, 80));
    }

    fwrite(STDOUT, $status[1] . "\n" . ($split[1] ?? ''));
}

/**
 * http-request, sent while this process holds the lock login.php takes to
 * check a password, as another login being checked would hold it. It waits
 * for any check already under way, and lets go once the answer is in.
 */
function httpRequestLocked(string $method, string $url, string $headers, string $body): void
{
    $lock = @fopen(STATS_LOGIN_LOCK, 'c');
    if ($lock === false || !flock($lock, LOCK_EX)) {
        throw new RuntimeException('cannot take the lock on ' . STATS_LOGIN_LOCK);
    }
    try {
        httpRequest($method, $url, $headers, $body);
    } finally {
        flock($lock, LOCK_UN);
        fclose($lock);
    }
}

/**
 * Prints "<kind> <access> <pid>" for every lock held on a file, as
 * /proc/locks lists it, and nothing if none is. It only reads: probing with
 * a lock of its own could take the very lock it looks for. /proc/locks names
 * a file by device and inode, the device as major:minor in hex.
 */
function lockHolders(string $path): void
{
    $stat = @stat($path);
    if ($stat === false) {
        throw new RuntimeException("cannot stat $path");
    }
    $device = $stat['dev'];
    $file = sprintf(
        '%02x:%02x:%d',
        ($device >> 8) & 0xfff,
        ($device & 0xff) | (($device >> 12) & 0xfff00),
        $stat['ino'],
    );

    // "1: FLOCK  ADVISORY  WRITE 1 00:2a:3 0 EOF"; a waiter's line has "->"
    // after the number, and holds nothing
    foreach (explode("\n", readOrThrow(PROC_LOCKS)) as $line) {
        $fields = preg_split('/\s+/', trim($line));
        if ($fields !== false && count($fields) >= 6 && $fields[1] !== '->' && $fields[5] === $file) {
            fwrite(STDOUT, "{$fields[1]} {$fields[3]} {$fields[4]}\n");
        }
    }
}

/**
 * Writes the stats password's hash at the bcrypt cost given, as the
 * bootstrap writes it at PHP's default: cost 16 makes each check take
 * seconds, so a check can be caught while it runs.
 */
function writeStatsHash(string $password, string $cost): void
{
    if (preg_match('/^(?:[4-9]|[12][0-9]|3[01])$/', $cost) !== 1) {
        throw new InvalidArgumentException("'$cost' is not a bcrypt cost");
    }
    $staging = STATS_PASSWORD_HASH . '.new';
    if (!@touch($staging)
        || !@chmod($staging, 0o600)
        || @file_put_contents($staging, password_hash($password, PASSWORD_BCRYPT, ['cost' => (int) $cost])) === false
        || !@rename($staging, STATS_PASSWORD_HASH)) {
        throw new RuntimeException('cannot write ' . STATS_PASSWORD_HASH);
    }

    $written = password_get_info(readOrThrow(STATS_PASSWORD_HASH))['options']['cost'] ?? null;
    Assertions::equals(STATS_PASSWORD_HASH . ', bcrypt cost', $cost, is_int($written) ? (string) $written : 'none');
}

/**
 * Holds SQLite's write lock on login.php's database for the given seconds,
 * as a long write would, printing "held" once it has it and "released" once
 * it lets go. A login that has to write waits the hold out.
 */
function holdDbLock(string $seconds): void
{
    if (preg_match('/^[1-9][0-9]?$/', $seconds) !== 1) {
        throw new InvalidArgumentException("'$seconds' is not a number of seconds");
    }
    $pdo = new PDO('sqlite:' . STATS_LOGIN_DB, null, null, [
        PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
        PDO::ATTR_TIMEOUT => HTTP_TIMEOUT,
    ]);
    $pdo->exec('BEGIN IMMEDIATE');
    fwrite(STDOUT, "held\n");
    sleep((int) $seconds);
    $pdo->exec('ROLLBACK');
    fwrite(STDOUT, "released\n");
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
        'assert-listeners' => ['arguments' => [], 'handler' => assertListeners(...)],
        'probe-egress' => ['arguments' => [], 'handler' => probeEgress(...)],
        'php-ini' => ['arguments' => [], 'handler' => phpIni(...)],
        'probe-getip' => ['arguments' => [], 'handler' => probeGetIp(...)],
        'damage-db' => ['arguments' => [], 'handler' => damageDb(...)],
        'remove-db' => ['arguments' => [], 'handler' => removeDb(...)],
        'read-salt' => ['arguments' => [], 'handler' => readSalt(...)],
        'read-row' => ['arguments' => ['id'], 'handler' => readRow(...)],
        'age-failures' => ['arguments' => ['seconds'], 'handler' => ageFailures(...)],
        'http-request' => [
            'arguments' => ['method', 'url', 'headers', 'body'],
            'handler' => httpRequest(...),
        ],
        'http-request-locked' => [
            'arguments' => ['method', 'url', 'headers', 'body'],
            'handler' => httpRequestLocked(...),
        ],
        'lock-holders' => ['arguments' => ['path'], 'handler' => lockHolders(...)],
        'write-stats-hash' => ['arguments' => ['password', 'cost'], 'handler' => writeStatsHash(...)],
        'hold-db-lock' => ['arguments' => ['seconds'], 'handler' => holdDbLock(...)],
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

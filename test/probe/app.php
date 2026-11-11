<?php

// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

declare(strict_types=1);

/*
 * LibreSpeed's own subcommands for divbast's in-container probe. The
 * harness installs this beside its core.php, which requires it and adds
 * what it returns to its own subcommands:
 *
 *     frankenphp php-cli core.php <subcommand> [arguments]
 *
 * php-ini and bootstrap-with print what they read or do, and assert
 * nothing. A subcommand given the wrong arguments throws, which core.php
 * records as a failure.
 */

final class LibreSpeedProbe
{
    /**
     * Every error level PHP 8.5 still raises, by bit. E_STRICT is gone: its
     * constant is deprecated and no error carries it.
     *
     * @var array<string, int>
     */
    private const array ERROR_LEVELS = [
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

    private const string SERVER_BIN = '/usr/local/bin/frankenphp';
    private const string BOOTSTRAP = '/usr/local/bin/librespeed-bootstrap';

    /**
     * Long enough for a bootstrap to refuse; one still running by then has
     * started the server
     */
    private const int BOOTSTRAP_DEADLINE = 10;

    /**
     * Prints, as JSON, the PHP settings the image depends on, as PHP itself
     * reads them in this SAPI.
     *
     * @param list<string> $operands
     */
    public static function phpIni(array $operands): void
    {
        self::operands('php-ini', $operands);
        $postMaxSize = (string) ini_get('post_max_size');

        fwrite(STDOUT, json_encode([
            'allow_url_fopen' => self::iniEnabled('allow_url_fopen'),
            'display_errors' => self::iniEnabled('display_errors'),
            'log_errors' => self::iniEnabled('log_errors'),
            'error_log' => (string) ini_get('error_log'),
            // error_reporting() is the level in force, E_ALL when nothing set it
            'error_reporting' => self::errorLevels(error_reporting()),
            'post_max_size' => $postMaxSize,
            // In bytes, as PHP reads the K, M and G suffixes
            'post_max_size_bytes' => ini_parse_quantity($postMaxSize),
        ], JSON_THROW_ON_ERROR) . "\n");
    }

    /**
     * Runs the bootstrap with each name in the JSON object set to the bytes
     * its hex value spells, which need not be UTF-8. An engine's -e cannot
     * carry such bytes: Docker sends the environment as JSON, which puts
     * U+FFFD in their place. Prints, as JSON, the exit code, or null if the
     * bootstrap was still running at the deadline and so was stopped, and
     * what it wrote to stderr.
     *
     * @param list<string> $operands
     */
    public static function bootstrapWith(array $operands): void
    {
        [$assignments] = self::operands('bootstrap-with', $operands, 'assignments');
        $values = json_decode($assignments, true, 2, JSON_THROW_ON_ERROR);
        if (!is_array($values)) {
            throw new RuntimeException('expected a JSON object of names and hex values');
        }
        $env = getenv();
        foreach ($values as $name => $hex) {
            $bytes = is_string($name) && is_string($hex) ? hex2bin($hex) : false;
            if ($bytes === false) {
                throw new RuntimeException("'$name' needs a name and an even run of hex digits");
            }
            $env[$name] = $bytes;
        }

        $process = proc_open(
            [self::SERVER_BIN, 'php-cli', self::BOOTSTRAP],
            [0 => ['file', '/dev/null', 'r'], 1 => ['file', '/dev/null', 'w'], 2 => ['pipe', 'w']],
            $pipes,
            null,
            $env,
        );
        if (!is_resource($process)) {
            throw new RuntimeException('cannot start ' . self::BOOTSTRAP);
        }
        stream_set_blocking($pipes[2], false);
        $stderr = '';
        $deadline = microtime(true) + self::BOOTSTRAP_DEADLINE;
        do {
            $stderr .= (string) stream_get_contents($pipes[2]);
            // exitcode is good only on the first call that finds it stopped
            $status = proc_get_status($process);
            if (!$status['running']) {
                break;
            }
            usleep(100_000);
        } while (microtime(true) < $deadline);
        if ($status['running']) {
            proc_terminate($process);
        }
        $stderr .= (string) stream_get_contents($pipes[2]);
        fclose($pipes[2]);
        proc_close($process);

        fwrite(STDOUT, json_encode(
            ['exit' => $status['running'] ? null : $status['exitcode'], 'stderr' => $stderr],
            JSON_THROW_ON_ERROR | JSON_INVALID_UTF8_SUBSTITUTE | JSON_UNESCAPED_SLASHES,
        ) . "\n");
    }

    /**
     * The operands, when there is one for each name, which core.php leaves
     * to the app to check.
     *
     * @param list<string> $operands
     *
     * @return list<string>
     */
    private static function operands(string $subcommand, array $operands, string ...$names): array
    {
        if (count($operands) !== count($names)) {
            throw new InvalidArgumentException(sprintf(
                '%s takes %s, got %d argument(s)',
                $subcommand,
                $names === [] ? 'no arguments' : implode(' ', array_map(static fn (string $n): string => "<$n>", $names)),
                count($operands),
            ));
        }

        return $operands;
    }

    /**
     * How PHP itself reads a boolean directive: on, yes and true, or else
     * the number the value starts with. display_errors also takes stderr
     * and stdout, and both mean on.
     */
    private static function iniEnabled(string $directive): bool
    {
        $value = strtolower(trim((string) ini_get($directive)));

        return in_array($value, ['on', 'yes', 'true', 'stderr', 'stdout'], true) || (int) $value !== 0;
    }

    /**
     * The levels a mask holds, by name, with any bit no level names left as
     * a number
     *
     * @return list<string>
     */
    private static function errorLevels(int $mask): array
    {
        $levels = [];
        foreach (self::ERROR_LEVELS as $name => $bit) {
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
}

return [
    'php-ini' => LibreSpeedProbe::phpIni(...),
    'bootstrap-with' => LibreSpeedProbe::bootstrapWith(...),
];

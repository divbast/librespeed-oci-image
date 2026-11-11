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
 * damage-db, remove-db, age-failures and write-stats-hash assert what
 * they did, in core.php's records. The rest print what they read or do,
 * and assert nothing. A subcommand given the wrong arguments throws, which
 * core.php records as a failure. Each may use what core.php defines: its
 * Assertions, readOrThrow() and httpRequest().
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

    private const string TELEMETRY_DB = '/data/telemetry.db';
    private const string SALT_FILE = '/data/idObfuscation_salt.php';
    /** The first bytes of every SQLite database file */
    private const string SQLITE_MAGIC = "SQLite format 3\0";
    /**
     * Where login.php records failed logins, and the lock it holds while it
     * checks a password
     */
    private const string STATS_LOGIN_DB = '/data/stats-login.db';
    private const string STATS_LOGIN_LOCK = '/tmp/librespeed/stats-login.lock';
    /** What login.php checks a password against, read on every request */
    private const string STATS_PASSWORD_HASH = '/data/stats-password.hash';
    /** Every lock the kernel holds, one per line, as fcntl(2) and flock(2) take them */
    private const string PROC_LOCKS = '/proc/locks';
    /** Every mount this process sees, one per line */
    private const string MOUNTINFO = '/proc/self/mountinfo';
    /** Seconds to wait for SQLite's lock on login.php's database */
    private const int DB_TIMEOUT = 10;
    /**
     * Seconds http-request-locked waits for login.php's lock, which a check
     * holds for as long as bcrypt takes, and how often it tries again.
     * Longer means a lock no one lets go of, and a suite that would
     * otherwise hang.
     */
    private const int LOCK_WITHIN = 30;
    private const int LOCK_RETRY_MICROSECONDS = 50_000;

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
     * Overwrites the database's header, so that SQLite no longer recognises
     * the file, as a disk fault or a half-restored backup would leave it.
     *
     * @param list<string> $operands
     */
    public static function damageDb(array $operands): void
    {
        self::operands('damage-db', $operands);
        $handle = @fopen(self::TELEMETRY_DB, 'r+b');
        if ($handle === false) {
            throw new RuntimeException('cannot open ' . self::TELEMETRY_DB . ' to damage it');
        }
        $written = fwrite($handle, str_repeat('x', strlen(self::SQLITE_MAGIC)));
        fclose($handle);

        clearstatcache(true, self::TELEMETRY_DB);
        $header = (string) @file_get_contents(self::TELEMETRY_DB, false, null, 0, strlen(self::SQLITE_MAGIC));
        Assertions::record(
            $written === strlen(self::SQLITE_MAGIC) && $header !== self::SQLITE_MAGIC,
            self::TELEMETRY_DB . ' no longer starts with the SQLite header',
            'damaged',
            $header === self::SQLITE_MAGIC ? 'intact' : 'damaged',
        );
    }

    /**
     * Removes the database and whatever journal SQLite left beside it.
     *
     * @param list<string> $operands
     */
    public static function removeDb(array $operands): void
    {
        self::operands('remove-db', $operands);
        foreach (['', '-journal', '-wal', '-shm'] as $suffix) {
            $path = self::TELEMETRY_DB . $suffix;
            if (file_exists($path)) {
                @unlink($path);
            }
        }

        clearstatcache(true, self::TELEMETRY_DB);
        $present = file_exists(self::TELEMETRY_DB);
        Assertions::record(!$present, self::TELEMETRY_DB . ' is gone', 'absent', $present ? 'present' : 'absent');
    }

    /**
     * Prints the salt file's contents and its mode, as JSON.
     *
     * @param list<string> $operands
     */
    public static function readSalt(array $operands): void
    {
        self::operands('read-salt', $operands);
        $contents = readOrThrow(self::SALT_FILE);
        $perms = @fileperms(self::SALT_FILE);
        if ($perms === false) {
            throw new RuntimeException('cannot stat ' . self::SALT_FILE);
        }

        fwrite(STDOUT, json_encode([
            'contents' => $contents,
            'mode' => sprintf('%04o', $perms & 0o7777),
        ], JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES) . "\n");
    }

    /**
     * Prints one stored result, by the database's own id, as JSON. It opens
     * the database read-only, so it never creates one that is missing.
     *
     * @param list<string> $operands
     */
    public static function readRow(array $operands): void
    {
        [$id] = self::operands('read-row', $operands, 'id');
        if (preg_match('/\A[1-9][0-9]*\z/', $id) !== 1) {
            throw new InvalidArgumentException("'$id' is not a row id");
        }
        if (!is_file(self::TELEMETRY_DB)) {
            throw new RuntimeException(self::TELEMETRY_DB . ' does not exist');
        }

        $statement = self::readOnly(self::TELEMETRY_DB)->prepare(
            'SELECT id, ip, ispinfo, extra, ua, lang, dl, ul, ping, jitter, log FROM speedtest_users WHERE id = ?',
        );
        $statement->execute([(int) $id]);
        $row = $statement->fetch(PDO::FETCH_ASSOC);
        if (!is_array($row)) {
            throw new RuntimeException("no row $id in " . self::TELEMETRY_DB);
        }

        fwrite(STDOUT, json_encode($row, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE) . "\n");
    }

    /**
     * Moves every failed login login.php has recorded the given number of
     * seconds into the past, so its window can be tested without waiting
     * it out. Moving none fails: a test that ages failures expects some.
     *
     * @param list<string> $operands
     */
    public static function ageFailures(array $operands): void
    {
        [$seconds] = self::operands('age-failures', $operands, 'seconds');
        if (preg_match('/\A[1-9][0-9]*\z/', $seconds) !== 1) {
            throw new InvalidArgumentException("'$seconds' is not a number of seconds");
        }
        if (!is_file(self::STATS_LOGIN_DB)) {
            throw new RuntimeException(self::STATS_LOGIN_DB . ' does not exist');
        }

        $statement = self::readWrite(self::STATS_LOGIN_DB)->prepare('UPDATE failures SET at = at - ?');
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
     * core.php's http-request, sent while this process holds the lock
     * login.php takes to check a password, as another login being checked
     * would hold it. It waits up to LOCK_WITHIN seconds for a check already
     * under way, and lets go once the answer is in, or the request fails.
     *
     * @param list<string> $operands
     */
    public static function httpRequestLocked(array $operands): void
    {
        [$method, $url, $headers, $body] = self::operands('http-request-locked', $operands, 'method', 'url', 'headers', 'body');
        $lock = @fopen(self::STATS_LOGIN_LOCK, 'c');
        if ($lock === false) {
            throw new RuntimeException('cannot open ' . self::STATS_LOGIN_LOCK);
        }
        try {
            $deadline = microtime(true) + self::LOCK_WITHIN;
            while (!flock($lock, LOCK_EX | LOCK_NB)) {
                if (microtime(true) >= $deadline) {
                    throw new RuntimeException(sprintf(
                        'cannot take the lock on %s within %d s: something holds it and never lets go',
                        self::STATS_LOGIN_LOCK,
                        self::LOCK_WITHIN,
                    ));
                }
                usleep(self::LOCK_RETRY_MICROSECONDS);
            }
            httpRequest($method, $url, $headers, $body);
        } finally {
            flock($lock, LOCK_UN);
            fclose($lock);
        }
    }

    /**
     * Prints "<kind> <access> <pid> <argv>" for every lock held on a file,
     * as /proc/locks lists it, with the holder's argv as a JSON list, or
     * null if it cannot be read; and nothing if no lock is held. It only
     * reads: probing with a lock of its own could take the very lock it
     * looks for. /proc/locks names a file by its filesystem's device, as
     * major:minor in hex, and its inode.
     *
     * @param list<string> $operands
     */
    public static function lockHolders(array $operands): void
    {
        [$path] = self::operands('lock-holders', $operands, 'path');
        $stat = @stat($path);
        if ($stat === false) {
            throw new RuntimeException("cannot stat $path");
        }
        $file = self::device($path) . ':' . $stat['ino'];

        // "1: FLOCK  ADVISORY  WRITE 1 00:2a:3 0 EOF"; a waiter's line has
        // "->" after the number, and holds nothing
        foreach (explode("\n", readOrThrow(self::PROC_LOCKS)) as $line) {
            $fields = preg_split('/\s+/', trim($line));
            if ($fields !== false && count($fields) >= 6 && $fields[1] !== '->' && $fields[5] === $file) {
                fwrite(STDOUT, "{$fields[1]} {$fields[3]} {$fields[4]} " . self::argv($fields[4]) . "\n");
            }
        }
    }

    /**
     * Writes the stats password's hash at the bcrypt cost given, as the
     * bootstrap writes it at PHP's default: cost 16 makes each check take
     * seconds, so a check can be caught while it runs.
     *
     * @param list<string> $operands
     */
    public static function writeStatsHash(array $operands): void
    {
        [$password, $cost] = self::operands('write-stats-hash', $operands, 'password', 'cost');
        if (preg_match('/\A(?:[4-9]|[12][0-9]|3[01])\z/', $cost) !== 1) {
            throw new InvalidArgumentException("'$cost' is not a bcrypt cost");
        }
        $staging = self::STATS_PASSWORD_HASH . '.new';
        if (!@touch($staging)
            || !@chmod($staging, 0o600)
            || @file_put_contents($staging, password_hash($password, PASSWORD_BCRYPT, ['cost' => (int) $cost])) === false
            || !@rename($staging, self::STATS_PASSWORD_HASH)) {
            throw new RuntimeException('cannot write ' . self::STATS_PASSWORD_HASH);
        }

        $written = password_get_info(readOrThrow(self::STATS_PASSWORD_HASH))['options']['cost'] ?? null;
        Assertions::equals(self::STATS_PASSWORD_HASH . ', bcrypt cost', $cost, is_int($written) ? (string) $written : 'none');
    }

    /**
     * Holds SQLite's write lock on login.php's database for the given
     * seconds, as a long write would, printing "held" once it has it and
     * "released" once it lets go. A login that has to write waits the hold
     * out, for as long as its own timeout allows.
     *
     * @param list<string> $operands
     */
    public static function holdDbLock(array $operands): void
    {
        [$seconds] = self::operands('hold-db-lock', $operands, 'seconds');
        if (preg_match('/\A[1-9][0-9]?\z/', $seconds) !== 1) {
            throw new InvalidArgumentException("'$seconds' is not a number of seconds");
        }
        $pdo = self::readWrite(self::STATS_LOGIN_DB);
        $pdo->exec('BEGIN IMMEDIATE');
        fwrite(STDOUT, "held\n");
        sleep((int) $seconds);
        $pdo->exec('ROLLBACK');
        fwrite(STDOUT, "released\n");
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
     * The device of the filesystem path is on, as /proc/locks writes it:
     * major:minor in hex. stat() can report another: btrfs gives each
     * subvolume a device of its own, while the lock is on the
     * filesystem's. mountinfo gives the filesystem's, for the mount
     * nearest path, the last of any mounted over one another.
     */
    private static function device(string $path): string
    {
        $real = realpath($path);
        if ($real === false) {
            throw new RuntimeException("cannot resolve $path");
        }
        $device = null;
        $longest = -1;
        // "36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw",
        // with white space in the mount point written in octal
        foreach (explode("\n", readOrThrow(self::MOUNTINFO)) as $line) {
            $fields = explode(' ', $line);
            if (count($fields) < 5 || preg_match('/\A[0-9]+:[0-9]+\z/', $fields[2]) !== 1) {
                continue;
            }
            $point = (string) preg_replace_callback(
                '/\\\\([0-3][0-7]{2})/',
                static fn (array $octal): string => chr((int) octdec($octal[1]) & 0xff),
                $fields[4],
            );
            $under = $point === '/' || $real === $point || str_starts_with($real, $point . '/');
            if ($under && strlen($point) >= $longest) {
                $device = $fields[2];
                $longest = strlen($point);
            }
        }
        if ($device === null) {
            throw new RuntimeException('no mount in ' . self::MOUNTINFO . " holds $real");
        }
        [$major, $minor] = explode(':', $device);

        return sprintf('%02x:%02x', (int) $major, (int) $minor);
    }

    /**
     * The process's argv, as /proc keeps it, as a JSON list, or null if it
     * cannot be read: the process is gone, or not this user's.
     */
    private static function argv(string $pid): string
    {
        $raw = preg_match('/\A[1-9][0-9]*\z/', $pid) === 1 ? @file_get_contents("/proc/$pid/cmdline") : false;
        if ($raw === false || $raw === '') {
            return 'null';
        }

        return json_encode(
            explode("\0", str_ends_with($raw, "\0") ? substr($raw, 0, -1) : $raw),
            JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_INVALID_UTF8_SUBSTITUTE,
        );
    }

    /**
     * A database, opened read-only, so a missing one is never created.
     */
    private static function readOnly(string $path): PDO
    {
        return new PDO('sqlite:' . $path, null, null, [
            PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
            Pdo\Sqlite::ATTR_OPEN_FLAGS => Pdo\Sqlite::OPEN_READONLY,
        ]);
    }

    /**
     * A database, opened to write, waiting for another writer as long as
     * login.php itself does.
     */
    private static function readWrite(string $path): PDO
    {
        return new PDO('sqlite:' . $path, null, null, [
            PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
            PDO::ATTR_TIMEOUT => self::DB_TIMEOUT,
        ]);
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
    'damage-db' => LibreSpeedProbe::damageDb(...),
    'remove-db' => LibreSpeedProbe::removeDb(...),
    'read-salt' => LibreSpeedProbe::readSalt(...),
    'read-row' => LibreSpeedProbe::readRow(...),
    'age-failures' => LibreSpeedProbe::ageFailures(...),
    'http-request-locked' => LibreSpeedProbe::httpRequestLocked(...),
    'lock-holders' => LibreSpeedProbe::lockHolders(...),
    'write-stats-hash' => LibreSpeedProbe::writeStatsHash(...),
    'hold-db-lock' => LibreSpeedProbe::holdDbLock(...),
];

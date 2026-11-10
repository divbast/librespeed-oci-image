<?php

// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

declare(strict_types=1);

/*
 * The endpoint half of the health definition, GET /healthz on :8081: what
 * the server needs, checked from inside the server, by the same PHP that
 * serves the speed test. rootfs/usr/local/bin/librespeed-health adds the
 * half only a request to :8080 can show.
 *
 * Every check runs, and each that fails is a line of the 503, its name and
 * why. No line names a filesystem path or repeats an exception's message.
 */

// getIP.php loads both from backend/, by these names
const GEOIP_PHAR = '/var/www/librespeed/backend/geoip2.phar';
const OFFLINE_DB = '/var/www/librespeed/backend/country_asn.mmdb';
const READER_CLASS = 'MaxMind\Db\Reader';
// Cloudflare's resolver, whose network is not about to change hands
const LOOKUP_ADDRESS = '1.1.1.1';
const LOOKUP_ASN = 'AS13335';
// What the bootstrap turned on for this start
const STATE_FILE = '/tmp/librespeed/state.json';
const TELEMETRY_DB = '/data/telemetry.db';

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
 * php/librespeed.ini, as this server applies it. Reading the live settings
 * proves the file was scanned, and that nothing scanned after it, from
 * PHP_INI_SCAN_DIR say, took any of them back: a URL stream would let PHP
 * reach out of the container, a displayed error would put a path in a
 * response, and with logging off no error would be seen at all.
 */
function checkIni(): ?string
{
    $wrong = array_keys(array_filter([
        'allow_url_fopen is on' => iniEnabled('allow_url_fopen'),
        'display_errors is on' => iniEnabled('display_errors'),
        'log_errors is off' => !iniEnabled('log_errors'),
    ]));

    return $wrong === [] ? null : implode(', ', $wrong);
}

/**
 * The phar holds the reader getIP.php opens the offline database with.
 * getIP.php requires it on every request, so one that would not load would
 * break every request for the client's address.
 */
function checkPhar(): ?string
{
    if (!is_file(GEOIP_PHAR) || !is_readable(GEOIP_PHAR)) {
        return 'missing or unreadable';
    }
    try {
        require_once GEOIP_PHAR;
    } catch (Throwable) {
        return 'does not load';
    }

    return class_exists(READER_CLASS) ? null : 'has no ' . READER_CLASS;
}

/**
 * The offline database, opened and asked for an address as getIP.php does,
 * with the fields getIP.php reads. A failed phar fails this too: without the
 * reader, getIP.php cannot use the database either.
 */
function checkMmdb(): ?string
{
    if (!class_exists(READER_CLASS)) {
        return 'no reader to open it with';
    }
    if (!is_file(OFFLINE_DB) || !is_readable(OFFLINE_DB)) {
        return 'missing or unreadable';
    }
    try {
        $reader = new MaxMind\Db\Reader(OFFLINE_DB);
        $record = $reader->get(LOOKUP_ADDRESS);
        $reader->close();
    } catch (Throwable) {
        return 'does not open';
    }

    return is_array($record) && ($record['asn'] ?? null) === LOOKUP_ASN
        && is_string($record['as_name'] ?? null) && is_string($record['country_name'] ?? null)
        ? null
        : LOOKUP_ADDRESS . ' is not ' . LOOKUP_ASN . ', with a name and a country';
}

/**
 * With telemetry on, the results database: there, writable, so a result is
 * recorded and not quietly dropped, and holding upstream's table. It is
 * opened read-only, so this never creates or changes what it checks:
 * upstream's own getPdo() would make a missing one.
 */
function checkDatabase(): ?string
{
    try {
        $state = json_decode((string) @file_get_contents(STATE_FILE), true, 4, JSON_THROW_ON_ERROR);
    } catch (JsonException) {
        $state = null;
    }
    if (!is_array($state) || !is_bool($state['telemetry'] ?? null)) {
        return 'no telling whether telemetry is on';
    }
    if (!$state['telemetry']) {
        return null;
    }

    if (!is_file(TELEMETRY_DB)) {
        return 'missing';
    }
    if (!is_writable(TELEMETRY_DB) || !is_writable(dirname(TELEMETRY_DB))) {
        return 'not writable';
    }
    try {
        $pdo = new PDO('sqlite:' . TELEMETRY_DB, null, null, [
            PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
            Pdo\Sqlite::ATTR_OPEN_FLAGS => Pdo\Sqlite::OPEN_READONLY,
        ]);
        $tables = $pdo->query("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'speedtest_users'");
        $found = $tables === false ? false : $tables->fetchColumn();
    } catch (Throwable) {
        return 'not a database SQLite can read';
    }

    return $found === 1 ? null : 'no speedtest_users table';
}

// Run in this order, which the mmdb check relies on: it opens the database
// with the reader the phar check loaded
$failures = array_filter([
    'ini' => checkIni(),
    'phar' => checkPhar(),
    'mmdb' => checkMmdb(),
    'database' => checkDatabase(),
], static fn (?string $why): bool => $why !== null);

http_response_code($failures === [] ? 200 : 503);
header('Content-Type: text/plain; charset=utf-8');
header('Cache-Control: no-store');
if ($failures === []) {
    echo "ok\n";
}
foreach ($failures as $check => $why) {
    echo "$check: $why\n";
}

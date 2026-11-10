<?php

// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

/*
 * The image's own login in front of upstream's results/stats.php. Caddy
 * sends here every request for stats.php that upstream could take for a
 * login: any with op=login in its query, and every POST at all. So upstream
 * never receives a password, and its own check against $stats_password
 * decides nothing.
 *
 * It checks the password against the hash the bootstrap writes from
 * STATS_PASSWORD, limits failures per client, and on success marks the
 * session the way stats.php reads it. It lives outside the served tree, and
 * Caddy reaches it only through that rewrite.
 */

declare(strict_types=1);

const PASSWORD_HASH_FILE = '/data/stats-password.hash';
const FAILURES_DB = '/data/stats-login.db';
// The first twelve bytes of an IPv4 address mapped into IPv6, ::ffff:a.b.c.d
const IPV4_MAPPED_PREFIX = "\0\0\0\0\0\0\0\0\0\0\xff\xff";
// Five failures for one client inside 900 s, and that client is refused
// until the oldest of them is 900 s old, even with the right password.
const FAILURE_LIMIT = 5;
const FAILURE_WINDOW = 900;
// bcrypt, which the hash is, reads no more than this, so a longer password
// that starts with the right one would otherwise be let in
const PASSWORD_MAX_BYTES = 72;
const STATS_PAGE = '/results/stats.php';
// Upstream's own form, which posts here by way of Caddy
const LOGIN_FORM = <<<'HTML'
    <form action="stats.php?op=login" method="POST">
        <h3>Login</h3>
        <input type="password" name="password" placeholder="Password" value=""/>
        <input type="submit" value="Login" />
    </form>
    HTML;

function respond(int $status, string $message, bool $form): never
{
    http_response_code($status);
    header('Content-Type: text/html; charset=utf-8');
    header('Cache-Control: no-store');
    echo "<!DOCTYPE html>\n<html>\n<head><meta charset=\"utf-8\"><title>LibreSpeed - Stats</title></head>\n",
        "<body>\n<h1>LibreSpeed - Stats</h1>\n<p>",
        htmlspecialchars($message, ENT_QUOTES | ENT_HTML5, 'UTF-8'),
        "</p>\n", $form ? LOGIN_FORM . "\n" : '', "</body>\n</html>\n";
    exit;
}

function redirectToStats(): never
{
    header('Cache-Control: no-store');
    header('Location: ' . STATS_PAGE, true, 303);
    exit;
}

/**
 * The client's address: {client_ip} as Caddy resolved it,
 * from the connection, or from X-Forwarded-For only when that connection is
 * from a proxy TRUSTED_PROXIES names. Caddy sets it as an environment
 * variable, which FrankenPHP registers after every request header, and a
 * header's own name always starts with HTTP_.
 */
function client(): string
{
    $client = $_SERVER['LIBRESPEED_CLIENT_IP'] ?? null;
    if (!is_string($client) || filter_var($client, FILTER_VALIDATE_IP) === false) {
        throw new RuntimeException('LIBRESPEED_CLIENT_IP is not an IP address, so failures cannot be counted');
    }

    return $client;
}

/**
 * What failures are counted against: an IPv4 address alone, an IPv6 address
 * by its /64, which is what one subscriber is usually given and holds more
 * addresses than any limit could count, and an IPv4-mapped IPv6 address as
 * the IPv4 address it is.
 */
function bucket(string $address): string
{
    $packed = inet_pton($address);
    if ($packed === false) {
        throw new RuntimeException("'$address' is not an IP address");
    }
    if (strlen($packed) === 16 && str_starts_with($packed, IPV4_MAPPED_PREFIX)) {
        $packed = substr($packed, 12);
    }
    $bucket = strlen($packed) === 4
        ? inet_ntop($packed)
        : inet_ntop(substr($packed, 0, 8) . str_repeat("\0", 8));
    if ($bucket === false) {
        throw new RuntimeException("cannot write '$address' back as an address");
    }

    return strlen($packed) === 4 ? $bucket : "$bucket/64";
}

function passwordHash(): string
{
    $hash = @file_get_contents(PASSWORD_HASH_FILE);
    if ($hash === false || $hash === '') {
        throw new RuntimeException('cannot read ' . PASSWORD_HASH_FILE);
    }

    return $hash;
}

function failures(): PDO
{
    $pdo = new PDO('sqlite:' . FAILURES_DB, null, null, [
        PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
        // Seconds to wait for another request's write to finish
        PDO::ATTR_TIMEOUT => 10,
    ]);
    $pdo->exec('CREATE TABLE IF NOT EXISTS failures (client TEXT NOT NULL, at INTEGER NOT NULL)');
    $pdo->exec('CREATE INDEX IF NOT EXISTS failures_by_client ON failures (client, at)');

    return $pdo;
}

/**
 * Counts the client's failures inside the window and, below the limit,
 * records this attempt as one more before the password is checked. A
 * success removes it again. Both happen in one write transaction, so
 * guesses sent in parallel cannot all pass the count before any is
 * recorded.
 *
 * Returns 0 when the attempt may go ahead, or else the seconds until the
 * oldest failure leaves the window.
 */
function admit(PDO $pdo, string $client, int $now): int
{
    $pdo->exec('BEGIN IMMEDIATE');
    try {
        $pdo->prepare('DELETE FROM failures WHERE at <= ?')->execute([$now - FAILURE_WINDOW]);
        $recent = $pdo->prepare('SELECT COUNT(*), MIN(at) FROM failures WHERE client = ?');
        $recent->execute([$client]);
        $row = $recent->fetch(PDO::FETCH_NUM);
        $count = is_array($row) && is_int($row[0]) ? $row[0] : 0;
        $oldest = is_array($row) && is_int($row[1]) ? $row[1] : $now;

        if ($count >= FAILURE_LIMIT) {
            $pdo->exec('COMMIT');

            return max(1, $oldest + FAILURE_WINDOW - $now);
        }

        $pdo->prepare('INSERT INTO failures (client, at) VALUES (?, ?)')->execute([$client, $now]);
        $pdo->exec('COMMIT');

        return 0;
    } catch (Throwable $problem) {
        if ($pdo->inTransaction()) {
            $pdo->exec('ROLLBACK');
        }

        throw $problem;
    }
}

function login(): never
{
    // A GET, as the address bar shows after a failed login, goes to the page
    if (($_SERVER['REQUEST_METHOD'] ?? '') !== 'POST') {
        redirectToStats();
    }

    $client = bucket(client());
    $hash = passwordHash();
    $pdo = failures();

    $wait = admit($pdo, $client, time());
    if ($wait > 0) {
        header('Retry-After: ' . $wait);
        respond(429, 'Too many failed logins from your address. Try again in '
            . (int) ceil($wait / 60) . ' minute(s).', false);
    }

    // $_POST, never php://input, which would spool a large body into /tmp
    $password = $_POST['password'] ?? null;
    if (!is_string($password) || strlen($password) > PASSWORD_MAX_BYTES || !password_verify($password, $hash)) {
        respond(403, 'Wrong password.', true);
    }

    $pdo->prepare('DELETE FROM failures WHERE client = ?')->execute([$client]);

    // A session id the client brought, perhaps planted by someone else, is
    // never the one logged in
    if (!session_start(['cookie_httponly' => true, 'cookie_samesite' => 'Lax'])
        || !session_regenerate_id(true)) {
        throw new RuntimeException('cannot start a new session');
    }
    $_SESSION['logged'] = true;
    session_write_close();

    redirectToStats();
}

try {
    login();
} catch (Throwable $problem) {
    error_log('librespeed login: ' . $problem->getMessage());
    respond(500, 'The login is not working. The server log says why.', false);
}

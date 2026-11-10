<?php

// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

declare(strict_types=1);

/*
 * What the image's own PHP borrows from the LibreSpeed tree, which the build
 * fetches and this repository never holds. Declarations only, for PHPStan
 * to scan: nothing here runs.
 *
 * rootfs/usr/local/bin/librespeed-bootstrap makes the database with getPdo().
 */

namespace {
    /**
     * results/telemetry_db.php, as LibreSpeed 6.3.1 ships it. Asked for an
     * error message, it returns one in place of false. It reads
     * telemetry_settings.php relative to the working directory, and with
     * SQLite runs CREATE TABLE IF NOT EXISTS on every connection it makes.
     *
     * @param bool $returnErrorMessage
     * @return PDO|string|false
     */
    function getPdo($returnErrorMessage = false)
    {
    }
}

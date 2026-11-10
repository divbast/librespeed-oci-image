#!/usr/bin/env bash
# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
set -euo pipefail

BIN="${1:?path to the frankenphp binary}"
PHP_EXTENSIONS="${2:?extensions the Containerfile asks for, comma separated}"
FAILED=0

# Modules every PHP 8.5 build carries whatever it is asked for, plus
# FrankenPHP's own. Every other module in the binary was asked for or came in
# as something's dependency, and either way it is code the image ships, so it
# must be named in PHP_EXTENSIONS.
CORE="core date frankenphp-cli hash json lexbor pcre random reflection spl standard uri"

fail() {
    echo "ERROR: ${1}" >&2
    FAILED=1
}

# php-cli -m answers only from PHP 8.6 on, so ask PHP itself.
loaded=$("${BIN}" php-cli -r 'echo implode("\n", get_loaded_extensions()), "\n";')

# get_loaded_extensions() names OPcache after its zend_extension entry, and
# capitalises a few others.
found=$(tr '[:upper:]' '[:lower:]' <<<"${loaded}" | sed 's/^zend opcache$/opcache/' | sort -u)
for module in ${CORE}; do
    found=$(grep -vxF "${module}" <<<"${found}" || true)
done
wanted=$(tr ',' '\n' <<<"${PHP_EXTENSIONS}" | tr '[:upper:]' '[:lower:]' | sed '/^$/d' | sort -u)

extra=$(comm -23 <(printf '%s\n' "${found}") <(printf '%s\n' "${wanted}"))
missing=$(comm -13 <(printf '%s\n' "${found}") <(printf '%s\n' "${wanted}"))
for module in ${extra}; do
    fail "the binary loads ${module}, which PHP_EXTENSIONS does not name."
done
for module in ${missing}; do
    fail "PHP_EXTENSIONS names ${module}, which the binary does not load."
done

# results/index.php draws the share image with imagefttext() and imagepng(),
# and nothing in LibreSpeed reads an image. So gd needs FreeType and PNG, and
# a JPEG, WebP or AVIF codec would be decoder code that nothing calls. One
# line per feature, "<name> <yes|no>", from gd's own report.
read -r -d '' report <<'PHP' || true
$info = function_exists('gd_info') ? gd_info() : [];
$features = ['FreeType Support' => 'freetype', 'PNG Support' => 'png',
    'JPEG Support' => 'jpeg', 'WebP Support' => 'webp', 'AVIF Support' => 'avif'];
foreach ($features as $key => $name) {
    echo $name, ' ', empty($info[$key]) ? 'no' : 'yes', "\n";
}
PHP
gd=$("${BIN}" php-cli -r "${report}") || true
for feature in freetype png; do
    if ! grep -qx "${feature} yes" <<<"${gd}"; then
        fail "gd_info() reports no ${feature} support, and the share image needs it."
    fi
done
for feature in jpeg webp avif; do
    if ! grep -qx "${feature} no" <<<"${gd}"; then
        fail "gd_info() reports ${feature} support, which nothing uses. Did craft.yml's with-suggests come back on?"
    fi
done

if ((FAILED)); then
    echo "Make PHP_EXTENSIONS name exactly what the binary loads, and" >&2
    echo "PHP_EXTENSION_LIBS give gd what it needs." >&2
    exit 1
fi
modules=$(tr '\n' ' ' <<<"${found}")
echo "the binary loads exactly PHP_EXTENSIONS, and gd has FreeType and PNG: ${modules}"

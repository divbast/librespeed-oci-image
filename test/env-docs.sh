#!/usr/bin/env bash
# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
set -euo pipefail
cd "$(dirname "${0}")/.."

# The README's Env Vars table is the operator's whole list of settings, so it
# must name exactly the variables the image reads: one it leaves out cannot
# be found, and one it names that nothing reads does nothing when set.
README="${README:-README.md}"
BOOTSTRAP="${BOOTSTRAP:-rootfs/usr/local/bin/librespeed-bootstrap}"
# The image's other PHP. None reads the environment today; a getenv() added
# to one is a setting too.
OTHERS=(
    rootfs/usr/local/bin/librespeed-health
    rootfs/usr/local/share/librespeed-health/index.php
    rootfs/usr/local/share/librespeed-login/login.php
)
FAILED=0

fail() {
    echo "ERROR: ${1}" >&2
    FAILED=1
}

# PHP's own tokenizer, so a comment or a string that only mentions a call is
# never taken for one, and a call written over several lines still is. The
# bootstrap reads its settings through envBool() and envString(), whose
# getenv($name) hides the name, so the name read is their first argument.
# Prints each name a string literal gives, one per line. Any other first
# argument hides a name from this check and the README alike, so it fails,
# with two exceptions: the helpers' own getenv($name), and getenv() with no
# argument, which copies the whole environment for the server.
read -r -d '' names <<'PHP' || true
$bootstrap = $argv[1];
$hidden = false;
foreach (array_slice($argv, 1) as $file) {
    $readers = $file === $bootstrap ? ['envbool', 'envstring', 'getenv'] : ['getenv'];
    $source = file_get_contents($file);
    if ($source === false) {
        fwrite(STDERR, "cannot read $file\n");
        exit(2);
    }
    $tokens = array_values(array_filter(token_get_all($source), static fn ($token): bool =>
        !is_array($token) || !in_array($token[0], [T_WHITESPACE, T_COMMENT, T_DOC_COMMENT], true)));
    $function = null;
    foreach ($tokens as $at => $token) {
        if (!is_array($token)) {
            continue;
        }
        $previous = $tokens[$at - 1] ?? null;
        if ($token[0] === T_STRING && is_array($previous) && $previous[0] === T_FUNCTION) {
            $function = strtolower($token[1]);
            continue;
        }
        $call = strtolower(ltrim($token[1], '\\'));
        if (!in_array($token[0], [T_STRING, T_NAME_FULLY_QUALIFIED], true) || !in_array($call, $readers, true)
            || ($tokens[$at + 1] ?? null) !== '('
            || (is_array($previous) && in_array($previous[0],
                [T_FUNCTION, T_OBJECT_OPERATOR, T_NULLSAFE_OBJECT_OPERATOR, T_DOUBLE_COLON], true))) {
            continue;
        }
        $argument = $tokens[$at + 2] ?? null;
        $after = $tokens[$at + 3] ?? null;
        if (is_array($argument) && $argument[0] === T_CONSTANT_ENCAPSED_STRING && ($after === ',' || $after === ')')) {
            echo substr($argument[1], 1, -1), "\n";
            continue;
        }
        if ($call === 'getenv' && $argument === ')') {
            continue;
        }
        if ($call === 'getenv' && in_array($function, ['envbool', 'envstring'], true)
            && is_array($argument) && $argument[0] === T_VARIABLE && $argument[1] === '$name' && $after === ')') {
            continue;
        }
        fwrite(STDERR, "ERROR: $file:{$token[2]}: {$token[1]}() reads a variable whose name is not written there\n");
        $hidden = true;
    }
}
exit($hidden ? 1 : 0);
PHP

if ! read_out=$(php -r "${names}" "${BOOTSTRAP}" "${OTHERS[@]}"); then
    fail "every variable the image reads must be named where it is read, as a string literal."
fi
read_names=$(sed '/^$/d' <<<"${read_out}" | sort -u)
if [[ -z "${read_names}" ]]; then
    fail "found no variable read in ${BOOTSTRAP}, so there is nothing to hold the README to."
fi

# The first cell of each row under "## Env Vars", up to the next heading,
# when it is a name in backticks
row="s/^\\| *\`([A-Za-z_][A-Za-z0-9_]*)\` *\\|.*/\\1/p"
documented=$(awk '/^## / { table = ($0 == "## Env Vars") } table' "${README}" | sed -nE "${row}")
if [[ -z "${documented}" ]]; then
    fail "${README} has no Env Vars table: no row under '## Env Vars' starts with a \`NAME\` cell."
fi
for name in $(sort <<<"${documented}" | uniq -d); do
    fail "${README}'s Env Vars table has more than one row for ${name}."
done
documented=$(sort -u <<<"${documented}")

for name in $(comm -23 <(printf '%s\n' "${read_names}") <(printf '%s\n' "${documented}")); do
    fail "the image reads ${name}, and ${README}'s Env Vars table does not name it."
done
for name in $(comm -13 <(printf '%s\n' "${read_names}") <(printf '%s\n' "${documented}")); do
    fail "${README}'s Env Vars table names ${name}, which the image never reads."
done

if ((FAILED)); then
    exit 1
fi
count=$(wc -l <<<"${read_names}" | tr -d ' ')
list=$(paste -sd ' ' - <<<"${read_names}")
echo "${README}'s Env Vars table names exactly the ${count} variables the image reads: ${list}"

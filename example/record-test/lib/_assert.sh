#!/usr/bin/env bash
# Test assertion helpers. Source this in tests/test_*.sh.
# Exposes: assert_eq, assert_match, install_mock_bin, cleanup_mocks.

_ASSERT_PASS=0
_ASSERT_FAIL=0
_MOCK_DIR=""

assert_eq() {
    local expected="$1" actual="$2" label="${3:-}"
    if [ "$expected" = "$actual" ]; then
        _ASSERT_PASS=$((_ASSERT_PASS + 1))
        echo "  ✓ ${label:-assert_eq}"
    else
        _ASSERT_FAIL=$((_ASSERT_FAIL + 1))
        echo "  ✗ ${label:-assert_eq}: expected [$expected], got [$actual]"
    fi
}

assert_match() {
    local pattern="$1" actual="$2" label="${3:-}"
    if echo "$actual" | grep -qE "$pattern"; then
        _ASSERT_PASS=$((_ASSERT_PASS + 1))
        echo "  ✓ ${label:-assert_match}"
    else
        _ASSERT_FAIL=$((_ASSERT_FAIL + 1))
        echo "  ✗ ${label:-assert_match}: pattern [$pattern] not in [$actual]"
    fi
}

# install_mock_bin <name> <script-body>
# Creates an executable at $_MOCK_DIR/<name> with the given body, and prepends to PATH.
install_mock_bin() {
    local name="$1" body="$2"
    if [ -z "$_MOCK_DIR" ]; then
        _MOCK_DIR=$(mktemp -d)
        export PATH="$_MOCK_DIR:$PATH"
    fi
    cat >"$_MOCK_DIR/$name" <<EOF
#!/usr/bin/env bash
$body
EOF
    chmod +x "$_MOCK_DIR/$name"
}

cleanup_mocks() {
    [ -n "$_MOCK_DIR" ] && rm -rf "$_MOCK_DIR"
}

report_results() {
    echo ""
    echo "Pass: $_ASSERT_PASS  Fail: $_ASSERT_FAIL"
    [ "$_ASSERT_FAIL" -eq 0 ]
}

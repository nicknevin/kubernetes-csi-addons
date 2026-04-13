# Git Pre-commit Hooks

This repository uses git pre-commit hooks to catch linting issues locally before they're pushed to the CI pipeline.

## Setup

### Automatic Setup (Recommended)
After cloning the repository, run:

```bash
./scripts/setup-hooks.sh
```

This will install all pre-commit hooks to `.git/hooks/` and make them executable.

### Manual Setup
If you prefer to set up manually:

```bash
cp scripts/hooks/* .git/hooks/
chmod +x .git/hooks/*
```

## Available Hooks

### pre-commit
Runs **shellcheck** on all staged shell scripts (`.sh` files) before allowing a commit.

**What it does:**
- Checks all modified/staged shell scripts for syntax and quality issues
- Prevents commits if any linting errors are found
- Shows detailed error messages to help you fix issues

**Example output:**
```
🔍 Running pre-commit shellcheck on shell scripts...
  Checking hack/run-replication-e2e.sh... ✓
  Checking test/e2e/utils/emergency-cleanup-iptables.sh... ✓
✅ All shell scripts passed linting!
```

**Example when there are errors:**
```
🔍 Running pre-commit shellcheck on shell scripts...
  Checking hack/example.sh... ✗ FAILED

In hack/example.sh line 22:
  local var=$(echo "test")
         ^-- SC2155: Declare and assign separately...

❌ Commit blocked: Fix shellcheck errors above and try again
```

## Bypassing Hooks (Skip Hook for Single Commit)

If you need to bypass the pre-commit hook for **a single commit** (not recommended), you can use:

```bash
git commit --no-verify
```

**Important**: The CI pipeline will still run the same checks and **fail if linting issues exist**. This should only be used as a temporary measure while you work on fixes.

**Example:**
```bash
git add .
git commit --no-verify -m "WIP: fixing linting issues"
# ... now run shellcheck and fix the issues
# Then commit again without --no-verify to verify it passes
```

**⚠️ Warning**: Using `--no-verify` repeatedly means your code won't be validated locally. Use only when necessary.

## Requirements

### Required Tools
- `git` - Version control
- `shellcheck` - Shell script linting

### Installing shellcheck
- **macOS**: `brew install shellcheck`
- **Ubuntu/Debian**: `apt-get install shellcheck`
- **Fedora/RHEL**: `dnf install ShellCheck`
- **Other**: See https://github.com/koalaman/shellcheck

If shellcheck is not installed, the hook will warn but not block commits.

## Testing Hooks Locally

To test if your shell scripts pass linting before committing:

```bash
# Check a specific file
shellcheck hack/run-replication-e2e.sh

# Check all shell files
find . -name "*.sh" -type f | xargs shellcheck
```

## Updating Hooks

When hooks are updated in the repository, reinstall them:

```bash
./scripts/setup-hooks.sh
```

## Disabling Hooks Temporarily (Multiple Commits)

If you need to **disable hooks for multiple commits** without verification:

### Option 1: Make Hook Non-Executable (Recommended for temporary work)

```bash
# Disable the hook
chmod -x .git/hooks/pre-commit

# Now perform your commits without hook checks
git add .
git commit -m "WIP: work in progress"
git commit -m "Continue working"

# Re-enable the hook when done
./scripts/setup-hooks.sh
```

**Verify the hook status:**
```bash
# Check if hook is executable (✓ enabled)
ls -la .git/hooks/pre-commit
# Expected: -rwxr-xr-x (executable)

# Disabled hook shows:
# Expected: -rw-r--r-- (not executable, no 'x')
```

### Option 2: Uninstall Hooks Temporarily

```bash
# Remove the installed hook
rm .git/hooks/pre-commit

# Perform commits without checking
git add .
git commit -m "WIP: work in progress"

# Reinstall the hook
./scripts/setup-hooks.sh
```

### Option 3: Use --no-verify for Each Commit

```bash
# Bypass hook for individual commits
git commit --no-verify -m "Commit message"
```

**When to use each option:**
| Scenario | Method | Command |
|----------|--------|---------|
| Single commit to bypass | `--no-verify` | `git commit --no-verify` |
| Multiple commits (dev branch) | Disable hook | `chmod -x .git/hooks/pre-commit` |
| Completely remove hook | Uninstall | `rm .git/hooks/pre-commit` |

## Re-enabling Hooks

After you're done working or ready to validate your code:

```bash
# Re-enable pre-commit hook
./scripts/setup-hooks.sh

# Verify it's now executable
ls -la .git/hooks/pre-commit
# Should show: -rwxr-xr-x
```

If you disabled the hook, run the setup script again to re-enable it.

**Best Practice**: Always re-enable hooks before pushing to avoid CI failures.

## Troubleshooting

### "shellcheck: command not found"
The hook warned about this but allowed the commit. Install shellcheck (see Requirements section).

### Hook not running
Verify it's executable:
```bash
ls -la .git/hooks/pre-commit
# Should show: -rwxr-xr-x (executable)
```

If not executable, run:
```bash
./scripts/setup-hooks.sh
```

### Commit blocked by hook, but I don't see the error
The error output may have been cut off. Run shellcheck manually:
```bash
shellcheck <your-file>.sh
```

## CI Pipeline

The same linting checks run in the GitHub Actions CI pipeline:
- Workflow: `.github/workflows/lint-extras.yaml`
- Check: "Lint Code Base" (uses super-linter/slim@v7)

**Key takeaway**: Fixing linting issues locally before committing prevents CI failures.

## Contributing

When adding new shell scripts:
1. Make sure they follow bash best practices
2. Run shellcheck before committing
3. The pre-commit hook will catch issues automatically
4. If you need to commit without fixes, use `git commit --no-verify` (but fix them before pushing)

## References

- [Shellcheck Wiki](https://www.shellcheck.net/)
- [Bash Best Practices](https://mywiki.wooledge.org/BashGuide)
- [Git Hooks Documentation](https://git-scm.com/book/en/v2/Customizing-Git-Git-Hooks)

# Git Pre-commit Hooks

This repository uses Git pre-commit hooks to catch linting issues locally before they're pushed to the CI pipeline.

## Setup

### Automatic Setup (Recommended)

After cloning the repository, run:

```bash
./scripts/setup-hooks.sh
```

This will install all pre-commit hooks to `.Git/hooks/` and make them executable.

### Manual Setup

If you prefer to set up manually:

```bash
cp scripts/hooks/* .Git/hooks/
chmod +x .Git/hooks/*
```

## Available Hooks

### pre-commit

Runs **shellcheck**, **yamllint**, **bash -n**, **shfmt**, **markdownlint**, and **Prettier** (`prettier@3.5.3` for Markdown, matching the GitHub Actions super-linter **MARKDOWN_PRETTIER** check) on staged files before allowing a commit.

**What it does:**

- Checks all modified/staged shell scripts (`.sh`) with shellcheck
- Checks all modified/staged shell scripts syntax with bash -n (BASH_EXEC)
- Checks all modified/staged shell scripts formatting with shfmt (SHELL_SHFMT)
- Checks all modified/staged YAML files (`.yaml`, `.yml`) with yamllint
- Checks all modified/staged Markdown files (`.md`) with markdownlint
- Checks all modified/staged Markdown files with Prettier (`npx prettier@3.5.3 --check`, same as CI **MARKDOWN_PRETTIER**)
- Prevents commits if any linting errors are found
- Shows detailed error messages to help you fix issues
- Only checks for linting tools if files of that type are present

**CI parity:** The workflow [lint-extras.yaml](../../.github/workflows/lint-extras.yaml) also runs **NATURAL_LANGUAGE** (textlint terminology) on Markdown; that check is not bundled in the hook. To match CI exactly locally, run the super-linter container with the same `FILTER_REGEX_EXCLUDE` and `VALIDATE_*` settings as that workflow.

**Example output:**

```plaintext
Running pre-commit linting checks...

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
Shellcheck (Shell scripts)
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  Checking hack/run-replication-end-to-end.sh... Pass
Shellcheck: 1 passed, 0 failed

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
Yamllint (YAML files)
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  Checking config/manager/kustomization.yaml... Pass
Yamllint: 1 passed, 0 failed

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
Linting Summary:
  Passed: 2
  Failed: 0
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
All files passed linting!
```

**Example when there are errors:**

```plaintext
Running pre-commit linting checks...

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
Shellcheck (Shell scripts)
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  Checking hack/example.sh... FAILED

In hack/example.sh line 22:
  local var=$(echo "test")
         ^-- SC2155: Declare and assign separately...

Shellcheck: 0 passed, 1 failed

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
Linting Summary:
  Passed: 0
  Failed: 1
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
BLOCKED: Commit blocked: Fix linting errors above and try again

Files with errors:
  - hack/example.sh
```

## Bypassing Hooks (Skip Hook for Single Commit)

If you need to bypass the pre-commit hook for **a single commit** (not recommended), you can use:

```bash
Git commit --no-verify
```

**Important**: The CI pipeline will still run the same checks and **fail if linting issues exist**. This should only be used as a temporary measure while you work on fixes.

**Example:**

```bash
Git add .
Git commit --no-verify -m "WIP: fixing linting issues"
# ... now run shellcheck and fix the issues
# Then commit again without --no-verify to verify it passes
```

**WARNING**: Using `--no-verify` repeatedly means your code won't be validated locally. Use only when necessary.

## Requirements

### Required Tools

- `Git` - Version control
- `shellcheck` - Shell script linting
- `yamllint` - YAML file linting

### Optional Tools (Recommended for Full Validation)

- `shfmt` - Shell script formatting checker
- `bash` - Shell syntax validator (usually pre-installed)
- `markdownlint` - Markdown file linting

### Installing shellcheck

- **macOS**: `brew install shellcheck`
- **Ubuntu/Debian**: `apt-get install shellcheck`
- **Fedora/RHEL**: `dnf install ShellCheck`
- **Other**: See [shellcheck on GitHub](https://github.com/koalaman/shellcheck)

### Installing yamllint

- **macOS**: `brew install yamllint`
- **Ubuntu/Debian**: `apt-get install yamllint`
- **Fedora/RHEL**: `dnf install yamllint`
- **Python/pip**: `pip install yamllint`
- **Other**: See [yamllint on GitHub](https://github.com/adrienverge/yamllint)

### Installing shfmt (Recommended)

- **macOS**: `brew install shfmt`
- **Ubuntu/Debian**: `apt-get install shfmt`
- **Fedora/RHEL**: `dnf install shfmt`
- **Go**: `go install mvdan.cc/sh/v3/cmd/shfmt@latest`

### Installing markdownlint (Recommended)

- **Node.js**: `npm install -g markdownlint`
- **Ubuntu/Debian**: May be available as `node-markdownlint`
- **Python**: `pip install markdownlint` (or `mdl`)

**Note**: The hook will warn if optional tools are not installed but will still allow commits. It's recommended to install all tools to catch issues locally before they reach the CI pipeline.

If either tool is not installed, the hook will warn but not block commits for that file type.

## Testing Hooks Locally

To test if your shell scripts pass linting before committing:

```bash
# Check a specific file
shellcheck hack/run-replication-end-to-end.sh

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
chmod -x .Git/hooks/pre-commit

# Now perform your commits without hook checks
Git add .
Git commit -m "WIP: work in progress"
Git commit -m "Continue working"

# Re-enable the hook when done
./scripts/setup-hooks.sh
```

**Verify the hook status:**

```bash
# Check if hook is executable (enabled)
ls -la .Git/hooks/pre-commit
# Expected: -rwxr-xr-x (executable)

# Disabled hook shows:
# Expected: -rw-r--r-- (not executable, no 'x')
```

### Option 2: Uninstall Hooks Temporarily

```bash
# Remove the installed hook
rm .Git/hooks/pre-commit

# Perform commits without checking
Git add .
Git commit -m "WIP: work in progress"

# Reinstall the hook
./scripts/setup-hooks.sh
```

### Option 3: Use --no-verify for Each Commit

```bash
# Bypass hook for individual commits
Git commit --no-verify -m "Commit message"
```

**When to use each option:**

| Scenario                      | Method        | Command                          |
| ----------------------------- | ------------- | -------------------------------- |
| Single commit to bypass       | `--no-verify` | `Git commit --no-verify`         |
| Multiple commits (dev branch) | Disable hook  | `chmod -x .Git/hooks/pre-commit` |
| Completely remove hook        | Uninstall     | `rm .Git/hooks/pre-commit`       |

## Re-enabling Hooks

After you're done working or ready to validate your code:

```bash
# Re-enable pre-commit hook
./scripts/setup-hooks.sh

# Verify it's now executable
ls -la .Git/hooks/pre-commit
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
ls -la .Git/hooks/pre-commit
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
- Check: "Lint codebase" (uses super-linter/slim@v7)

**Key takeaway**: Fixing linting issues locally before committing prevents CI failures.

## Contributing

When adding new shell scripts:

1. Make sure they follow bash best practices
2. Run shellcheck before committing
3. The pre-commit hook will catch issues automatically
4. If you need to commit without fixes, use `Git commit --no-verify` (but fix them before pushing)

## References

- [Shellcheck Wiki](https://www.shellcheck.net/)
- [Bash Best Practices](https://mywiki.wooledge.org/BashGuide)
- [Git Hooks Documentation](https://Git-scm.com/book/en/v2/Customizing-Git-Git-Hooks)

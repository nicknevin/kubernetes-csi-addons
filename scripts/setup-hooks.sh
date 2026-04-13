#!/bin/bash
# Setup script: Install git pre-commit hooks for the repository
# Run this after cloning: ./scripts/setup-hooks.sh
# Or manually: cp scripts/hooks/** .git/hooks/ && chmod +x .git/hooks/*

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
HOOKS_SRC_DIR="$REPO_ROOT/scripts/hooks"
HOOKS_DEST_DIR="$REPO_ROOT/.git/hooks"

echo "Setting up git pre-commit hooks..."

# Check if hooks directory exists
if [ ! -d "$HOOKS_SRC_DIR" ]; then
	echo "ERROR: Hooks directory not found at $HOOKS_SRC_DIR"
	exit 1
fi

if [ ! -d "$HOOKS_DEST_DIR" ]; then
	echo "ERROR: .git/hooks directory not found. Are you in a git repository?"
	exit 1
fi

# Copy and make executable all hook scripts
for hook_file in "$HOOKS_SRC_DIR"/*; do
	if [ -f "$hook_file" ]; then
		hook_name=$(basename "$hook_file")
		dest_path="$HOOKS_DEST_DIR/$hook_name"

		echo -n "  Installing $hook_name... "
		cp "$hook_file" "$dest_path"
		chmod +x "$dest_path"
		echo "OK"
	fi
done

echo ""
echo "Git hooks installed successfully!"
echo ""
echo "Available hooks:"
find "$HOOKS_SRC_DIR" -maxdepth 1 -type f -exec basename {} \; | sed 's/^/  - /'
echo ""
echo "Tips:"
echo "  - To bypass hooks for a commit: git commit --no-verify"
echo "  - To update hooks: run setup-hooks.sh again"
echo "  - Required tools: shellcheck, yamllint, shfmt; Node.js (npx) for Prettier on Markdown"
echo "  - Hooks location: $HOOKS_DEST_DIR"

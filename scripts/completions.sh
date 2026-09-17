#!/bin/sh
# Generates shell completions into build/completions (packed into the release archives).
set -e
rm -rf build/completions && mkdir -p build/completions
for sh in bash zsh fish; do
  case $sh in
    bash) out=build/completions/iugu.bash ;;
    zsh)  out=build/completions/_iugu ;;
    fish) out=build/completions/iugu.fish ;;
  esac
  go run ./cmd/iugu completion "$sh" > "$out"
done

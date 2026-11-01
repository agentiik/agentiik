#!/bin/sh
# Copies the brick templates of agentiik/brick-sdk here, each file under its own name with .tmpl
# added: a go.mod among them would make its directory a module of its own, which go:embed refuses,
# and a main.go would be built as part of this one.
#
#     cmd/agk/internal/templates/copy.sh ../brick-sdk
#
# They are a copy and not a fetch, so that agk brick init works with no network and writes what
# the binary was released with. Before a release the copy is taken again from brick-sdk at the
# same version, as RELEASING.md lists.
set -eu
sdk=${1:?the path of an agentiik/brick-sdk checkout}
here=$(cd "$(dirname "$0")" && pwd)
for lang in go python typescript; do
  rm -rf "${here:?}/$lang"
  (cd "$sdk/templates/$lang" && find . -type f) | while read -r f; do
    mkdir -p "$(dirname "$here/$lang/$f")"
    cp "$sdk/templates/$lang/$f" "$here/$lang/$f.tmpl"
  done
done

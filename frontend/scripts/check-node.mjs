// Fail fast (and legibly) when the frontend is built with an unsupported
// Node.js. Vite 8 needs Node 20.19+ or 22.12+; on 18.x it dies deep inside
// its own CLI with "ReferenceError: CustomEvent is not defined", which says
// nothing about how to fix it.
//
// Wired as the `prebuild` / `predev` npm script, so `npm run build` stops
// here instead of crashing later. Use `scripts/with-node.sh` (or `make
// build`) from the repo root to pick a compatible Node automatically.

const REQUIREMENT = 'Node 20.19+ or 22.12+'

const [major, minor] = process.versions.node.split('.').map(Number)

const supported =
  major > 22 || (major === 22 && minor >= 12) || (major === 20 && minor >= 19)

if (!supported) {
  console.error(`
✗ Node.js v${process.versions.node} is too old for this frontend build.
  Required: ${REQUIREMENT} (Vite 8).

  Fixes:
    - from the repo root:  make build            # resolves a usable Node for you
    - or:                  scripts/with-node.sh npm run build
    - or install one:      nvm install --lts && nvm use --lts
    - or pin an existing one: NOVA_NODE_BIN=/path/to/node/bin make build
`)
  process.exit(1)
}

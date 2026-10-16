# CodexRelay public lab

Synthetic policy lab. The actual Go evaluator runs in your browser. No accounts, credentials, proxy requests, or model calls.

Build from the repository root with `bash docs/public-demo/build.sh`, then serve `docs/public-demo` with a static HTTP server. Generated Go WASM assets are ignored; use the matching Go toolchain wasm_exec.js. Publish only this directory as the Vercel project root, using its vercel.json. It contains no application API or service credentials.

This is a policy preview using invented evidence, not a connection to Codex or a demonstration of live subscription quota.

Input limits are enforced by the local demo engine. Reset or reload discards state. CSP limits scripts, workers, and connections to this origin. No dependencies are loaded from a CDN.

After building, run `node docs/public-demo/check.cjs` from the repository root. It exercises the compiled native engine with positive and rejection controls without a server or network call.

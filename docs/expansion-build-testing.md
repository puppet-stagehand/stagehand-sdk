# expansion-build: human testing guide

Automated tests cover the builder with stub `react` and `@stagehand/console-ui`
modules. These steps need the real packages, a real console or a person's eyes.
Record the result of each in the box.

Setup for all steps: a pack directory with a valid `manifest.json` (zero
placeholder `ui_digest`), `ui/src/index.tsx`, `ui/src/slots.json`, and a
`node_modules` that holds the real `react`, `react-dom` and `@stagehand/console-ui`.

## 1. Build a real pack

Do: write a `nodeDetailTab` that renders a `Card` from the real `@stagehand/console-ui`. Run `expansion-build ui`, then `pack-check --ui dist/ui manifest.json`.
Expect: both exit 0; `dist/ui/index.js` imports `/runtime/v1/console-ui.js` and `/runtime/v1/jsx-runtime.js` and contains no `react` import; `manifest.json` shows a new `ui_digest` and is otherwise unchanged (`git diff`).
Result: [ ] pass [ ] fail

## 2. Load it in the console

Do: serve the built `ui/` from the pack's worker (or the console's phase 55 loader fixture) and open a node's detail page.
Expect: a tab labelled exactly as in `slots.json`; its content renders using the console's theme; no console errors.
Result: [ ] pass [ ] fail

## 3. A changed byte changes the fingerprint

Do: change one character of visible text in `index.tsx`, rebuild, and compare `ui_digest` before and after. Then reinstall the new build over the old one in the console.
Expect: the digest differs; the console accepts the new build with the new digest and would refuse the new files under the old digest.
Result: [ ] pass [ ] fail

## 4. The house rules refuse

Do: add `const c = "#fff";` to `index.tsx` and rebuild. Repeat with an emoji, `font-family: Arial` in a string, and the word "converged".
Expect: each run exits 1 with the file, line and a fix line, and `dist/ui` and `manifest.json` are untouched (`git status`, file timestamps).
Result: [ ] pass [ ] fail

## 5. A careless --out is refused

Do: make a folder with an unrelated file in it and pass it as `--out`. Then pass the pack root as `--out`.
Expect: `ui_out_unsafe` both times; the unrelated file and your sources are still there.
Result: [ ] pass [ ] fail

## 6. A sandbox build is self-contained

Do: add `ui/src/sandbox.tsx` using the real `react` and `react-dom/client`. Rebuild.
Expect: `dist/ui/sandbox.js` exists, has no `import` statements, and is listed as `sandbox_entry` in `ui.manifest.json`; loading it in the sandbox frame renders the slot.
Result: [ ] pass [ ] fail

## 7. Same sources, same bytes

Do: build twice into two different `--out` folders, on two different machines if you can.
Expect: identical `ui.manifest.json` and identical `ui_digest`. If the machines differ, note the Go and esbuild versions.
Result: [ ] pass [ ] fail

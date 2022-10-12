# qutest

A standalone CLI to run [QUnit](QUnit) based tests using the
[Chrome DevTools Protocol](ChromeDP).

## Goals

- Be fast.
- Run without Node.
- Code Coverage reporting.
- Snapshot support.

## TODO

- hook into console.log and friends
- get single test file to bundle with esbuild and test
- log test count and time metrics
- include outside timer + runEnd timer
- walkdir and run tests as we find them
- collect coverage (across tests)
- replace QUnit?
- support expect style tests?
- screenshot support?

[qunit]: https://qunitjs.com/
[chromedp]: https://chromedevtools.github.io/devtools-protocol/

# qutest

A standalone CLI to run [QUnit](QUnit) based tests using the
[Chrome DevTools Protocol](ChromeDP).

## Goals

- Be fast.
- Run without Node.
- Code Coverage reporting.
- Snapshot support.
- Support inline tests with code.

## TODO

- snapshot testing
- hook into console.log and friends
- implement global timeout
- make failed tests include stack trace
- show pretty diff in comparison failures
- collect coverage (across tests)
- replace QUnit?
- support expect style tests?
- screenshot support?

[qunit]: https://qunitjs.com/
[chromedp]: https://chromedevtools.github.io/devtools-protocol/

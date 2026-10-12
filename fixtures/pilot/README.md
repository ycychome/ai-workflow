# AI Workflow Pilot Fixture

This standalone Go module is the first GitHub pilot repository:
`github.com/ycychome/ai-workflow-pilot`. Its first Issue is **Repair `Add`**.
The defect is intentional: `Add` currently subtracts its arguments, and the
included test expects ordinary integer addition. A failing baseline is the
expected starting condition for that Issue.

Run the fixture locally with:

```sh
go test ./...
```

The initial test run should fail at `TestAdd`. Do not treat that baseline
failure as a passed verification. A successful repair must make the test pass
without weakening or deleting its assertions.

## Pilot Issue queue

Each entry is a separate, bounded follow-up Issue with explicit acceptance
criteria. Keep the first run focused on item 1.

1. **Repair `Add`.** Acceptance: `Add(a, b)` returns the mathematical sum for
   positive, negative, and zero operands; the existing test passes; the public
   function signature remains unchanged.
2. **Add checked addition.** Acceptance: add `AddChecked(a, b int) (int, error)`;
   it returns the sum when representable and a non-nil error on both positive
   and negative integer overflow; test architecture-sized integer boundaries.
3. **Add checked multiplication.** Acceptance: add
   `MultiplyChecked(a, b int) (int, error)`; ordinary products are correct,
   overflow returns an error, and zero/negative operands are covered.
4. **Specify division behavior.** Acceptance: document that `Divide` uses
   integer division truncated toward zero; test positive and negative
   quotients and confirm the function signature does not change.
5. **Cover division edge cases.** Acceptance: `Divide` returns an error for a
   zero divisor, never panics for that case, and has a documented/tested result
   for the minimum integer divided by `-1` on the current architecture.
6. **Test whitespace normalization.** Acceptance: `Normalize` trims leading
   and trailing Unicode whitespace and converts every internal whitespace run
   to one ASCII space; include tabs, newlines, and non-ASCII spaces.
7. **Test case normalization.** Acceptance: `Normalize` lowercases input using
   Go's Unicode-aware lowercase conversion and preserves non-whitespace
   punctuation; add table-driven examples.
8. **Document `Normalize`.** Acceptance: exported documentation states the
   whitespace and case behavior, and examples in package documentation match
   the implementation.
9. **Add a public API example.** Acceptance: add a runnable `ExampleNormalize`
   (or an example for a different exported function) whose output is checked
   by `go test` and whose source compiles with the standard library only.
10. **Add arithmetic property tests.** Acceptance: add deterministic tests for
    commutativity of `Add` and `Multiply` over a safe bounded input range;
    generated values must avoid overflowing `int` and test failures must print
    the counterexample operands.

Items 2–10 are candidate pilot work, not claims that those features have
already been implemented or verified.

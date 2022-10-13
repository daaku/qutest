const add42 = (v: number) => v + 42

QUnit.test('add42', assert => {
  assert.equal(add42(1), 43)
})

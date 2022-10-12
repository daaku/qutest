const add42 = (v: number) => v + 42

QUnit.test('add42', assert => {
  assert.equals(add42(1), 43)
})

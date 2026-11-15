import assert from 'node:assert/strict'
import { test } from 'node:test'
import { androidMetaState } from '../src/media/keyModifiers.ts'

test('NumLock enables Android numeric keypad metadata without leaking lock state', () => {
  const modifiers = { shiftKey: false, altKey: false, ctrlKey: false, metaKey: false }
  assert.equal(androidMetaState({ ...modifiers, getModifierState: key => key === 'NumLock' }), 0x200000)
  assert.equal(androidMetaState({ ...modifiers, getModifierState: () => false }), 0)
})

test('NumLock preserves Shift, Alt, Ctrl and Meta combinations', () => {
  const modifiers = { shiftKey: true, altKey: true, ctrlKey: true, metaKey: true }
  assert.equal(androidMetaState({ ...modifiers, getModifierState: key => key === 'NumLock' }), 0x211003)
  assert.equal(androidMetaState({ ...modifiers, getModifierState: () => false }), 0x11003)
})

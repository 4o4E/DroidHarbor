type KeyModifiers = Pick<KeyboardEvent, 'shiftKey' | 'altKey' | 'ctrlKey' | 'metaKey' | 'getModifierState'>

export function androidMetaState(event: KeyModifiers): number {
  // Android NUMPAD keys are navigation keys unless META_NUM_LOCK_ON is set.
  return (event.shiftKey ? 1 : 0) | (event.altKey ? 2 : 0)
    | (event.ctrlKey ? 0x1000 : 0) | (event.metaKey ? 0x10000 : 0)
    | (event.getModifierState('NumLock') ? 0x200000 : 0)
}

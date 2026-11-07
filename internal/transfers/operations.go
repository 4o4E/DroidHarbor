package transfers

import "context"

// Cancel interrupts the operation and waits until its local staging file and
// ownership journal have reached a terminal state.
func (m *Manager) Cancel(ctx context.Context, owner, transferID string) error {
	t, err := m.authorizedTransfer(ctx, owner, transferID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if terminal(t.value.State) {
		err := t.finishErr
		m.mu.Unlock()
		if err != nil {
			return finalizationProblem(err)
		}
		return problem(409, "TRANSFER_TERMINAL", "文件传输已经结束", nil)
	}
	t.cancel()
	done := t.done
	m.mu.Unlock()
	select {
	case <-done:
		m.mu.Lock()
		err := t.finishErr
		m.mu.Unlock()
		return finalizationProblem(err)
	case <-ctx.Done():
		return ctx.Err()
	}
}
func finalizationProblem(err error) error {
	if err == nil {
		return nil
	}
	return problem(503, "TRANSFER_FINALIZATION_FAILED", "无法完成文件传输清理", err)
}

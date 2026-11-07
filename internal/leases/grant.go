package leases

import "context"

// Grant pins one lease and one observed link incarnation. Its packet-path check
// reads cancellation and the current expiry ticket without taking the global
// lease/SQLite mutex. Renewal replaces the ticket, not the pinned link.
type Grant struct {
	context.Context
	link    Link
	manager *Manager
	state   *leaseState
	cancel  context.CancelFunc
}

func (g *Grant) Link() Link { return g.link }

// Close abandons this access grant, not the lease or any sibling grant.
func (g *Grant) Close() { g.cancel() }

func (g *Grant) Check() error {
	for {
		if g.manager.ctx.Err() != nil {
			return ErrClosed
		}
		if g.state.ctx.Err() != nil {
			return ErrLeaseEnded
		}
		if err := g.Context.Err(); err != nil {
			return err
		}
		ticket := g.state.expiry.Load()
		if ticket == nil {
			return ErrLeaseEnded
		}
		if !g.manager.now().Before(ticket.deadline) {
			if g.manager.expireTicket(g.state, ticket) {
				return ErrLeaseEnded
			}
			// A concurrently committed renewal may have replaced the old ticket.
			continue
		}
		if !g.manager.source.HasLink(g.link) {
			g.cancel()
			return ErrDeviceOffline
		}
		if g.state.expiry.Load() != ticket {
			continue
		}
		if g.state.ctx.Err() != nil {
			return ErrLeaseEnded
		}
		return g.Context.Err()
	}
}

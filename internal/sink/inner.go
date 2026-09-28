package sink

// Unwrap is the sink the ledger sits in front of, for a caller that needs
// what only that sink can do, such as a migration dropping a table.
func (u *Unchanged) Unwrap() Sink { return u.inner }

// Inner is the sink behind any ledger wrapped around it.
func Inner(s Sink) Sink {
	for {
		u, ok := s.(interface{ Unwrap() Sink })
		if !ok {
			return s
		}
		s = u.Unwrap()
	}
}

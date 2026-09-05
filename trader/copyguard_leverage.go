package trader

import "nofx/decision"

func copyLeverageReceipt(d *decision.Decision, clientID string) func(int) {
	if d == nil || d.OnLeverageConfirmed == nil {
		return nil
	}
	return func(leverage int) { d.OnLeverageConfirmed(clientID, leverage) }
}

func notifyLeverageConfirmed(leverage int, observers []func(int)) {
	for _, observer := range observers {
		if observer != nil {
			observer(leverage)
		}
	}
}

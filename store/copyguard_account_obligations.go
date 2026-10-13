package store

// unsettledAccountProtectionSQL expects the protective-order alias o. An
// invalid row means the venue confirmed absence, but it releases the account
// only after its own cycle closed and no replacement identity remains. Keep
// the original identities and status for audit; this is not a cancellation.
const unsettledAccountProtectionSQL = `(o.replacement_pending=1
 OR COALESCE(o.previous_algo_id,'')<>'' OR COALESCE(o.previous_algo_client_id,'')<>'' OR (
 LOWER(COALESCE(o.status,'')) NOT IN ('canceled','cancelled','effective','filled','triggered','order_failed','failed','expired')
 AND (COALESCE(o.algo_id,'')<>'' OR COALESCE(o.algo_client_id,'')<>'')
 AND NOT (
  LOWER(COALESCE(o.status,''))='invalid' AND o.replacement_pending=0
  AND COALESCE(o.previous_algo_id,'')='' AND COALESCE(o.previous_algo_client_id,'')=''
  AND EXISTS (SELECT 1 FROM copy_guard_cycles retired
   WHERE retired.id=o.cycle_id AND retired.trader_id=o.trader_id AND retired.closed_at IS NOT NULL)
  )
 ))`

// Both startup and legacy-archive recovery must agree on which historical
// records still claim an account. Expects trader alias t.
func unsettledTraderVenueInstructionsSQL() string {
	return `(EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts o JOIN copy_trade_execution_intents oi ON oi.id=o.intent_id WHERE oi.trader_id=t.id AND ` + unsettledExecutionAttemptSQL("o") + `)
 OR EXISTS(SELECT 1 FROM copy_trade_execution_intents i WHERE i.trader_id=t.id AND (i.submitted_at IS NOT NULL OR COALESCE(i.exchange_order_id,'')<>'') AND (i.terminal_at IS NULL OR UPPER(COALESCE(i.exchange_state,'')) NOT IN ('FILLED','CANCELED','CANCELLED','REJECTED','EXPIRED','FAILED')) AND NOT EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts o WHERE o.intent_id=i.id) AND NOT ` + retiredLegacyVenueObligationSQL + `)
 OR EXISTS(SELECT 1 FROM copy_guard_protective_orders o WHERE o.trader_id=t.id AND ` + unsettledAccountProtectionSQL + `))`
}

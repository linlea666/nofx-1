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

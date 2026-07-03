package trader

import "testing"

func TestValidateOKXAlgoAckChecksPerItemResult(t *testing.T) {
	if err := validateOKXAlgoAck([]byte(`[{"algoId":"1","sCode":"0","sMsg":""}]`), "amend"); err != nil {
		t.Fatalf("successful acknowledgement rejected: %v", err)
	}
	if err := validateOKXAlgoAck([]byte(`[{"algoId":"1","sCode":"51000","sMsg":"invalid quantity"}]`), "amend"); err == nil {
		t.Fatal("per-item rejection must be returned as an error")
	}
	if err := validateOKXAlgoAck([]byte(`[]`), "cancel"); err == nil {
		t.Fatal("empty acknowledgement must be rejected")
	}
}

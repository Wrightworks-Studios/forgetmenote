package automation

type RetryPolicy struct {
	MaxAttempts  int `json:"maxAttempts"`
	DelaySeconds int `json:"delaySeconds"`
}

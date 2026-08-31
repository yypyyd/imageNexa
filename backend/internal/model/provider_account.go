package model

// TokenAccount is retained as the adapter-facing Go type name while the
// durable product concept and table are provider_accounts.
func (TokenAccount) TableName() string { return "provider_accounts" }

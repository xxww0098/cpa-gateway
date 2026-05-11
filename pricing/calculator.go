package pricing

// Calculator is a placeholder pricing calculator type.
//
// Task 6c introduces this type so api.PanelRouter can declare a typed
// dependency (`Calc *pricing.Calculator`). The full Estimate/Compute
// implementation — along with `UsageTokens` and `ModelPriceCache`
// integration — lands in Wave 2 Task 8.
//
// Downstream handlers in api/ do not yet consume any Calculator methods,
// so the zero value is a safe default for Wave 1 wiring.
type Calculator struct{}

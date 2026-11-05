package domain

import "strings"

// SystemAccount names a platform-owned account that the transfer operations
// look up by purpose and currency.
type SystemAccount string

// System accounts of the marketplace chart.
const (
	SysBank           SystemAccount = "assets:bank"
	SysPSPReceivable  SystemAccount = "assets:psp-receivable"
	SysSuspense       SystemAccount = "assets:suspense"
	SysRefundsPayable SystemAccount = "liabilities:refunds-payable"
	SysOwnerCapital   SystemAccount = "equity:owner-capital"
	SysFeeRevenue     SystemAccount = "revenue:fees"
	SysPSPFees        SystemAccount = "expenses:psp-fees"
	SysRoundingDiff   SystemAccount = "expenses:rounding"
)

// Code returns the account code of the system account for a currency, for
// example "assets:bank:idr".
func (s SystemAccount) Code(currency string) string {
	return string(s) + ":" + strings.ToLower(currency)
}

// ChartAccount is one row of the default chart of accounts.
type ChartAccount struct {
	System         SystemAccount
	Name           string
	Type           AccountType
	AllowOverdraft bool
}

// DefaultChart is the chart of accounts seeded for every enabled currency.
// Customer wallets and merchant payables are created per party through the
// accounts API, with codes such as "liabilities:wallets:cust-1001:idr".
var DefaultChart = []ChartAccount{
	{SysBank, "Platform bank account", AccountAsset, false},
	{SysPSPReceivable, "Receivable from payment processor", AccountAsset, true},
	{SysSuspense, "Suspense (unidentified money)", AccountAsset, true},
	{SysRefundsPayable, "Refunds payable to original payment method", AccountLiability, false},
	{SysOwnerCapital, "Owner capital", AccountEquity, true},
	{SysFeeRevenue, "Platform fee revenue", AccountRevenue, true},
	{SysPSPFees, "Payment processor fees", AccountExpense, true},
	{SysRoundingDiff, "Rounding differences", AccountExpense, true},
}

// ChartFor returns the default chart accounts for one currency.
func ChartFor(currency string) []*Account {
	out := make([]*Account, 0, len(DefaultChart))
	for _, c := range DefaultChart {
		out = append(out, &Account{
			Code:           c.System.Code(currency),
			Name:           c.Name + " (" + currency + ")",
			Type:           c.Type,
			Currency:       currency,
			Status:         StatusActive,
			AllowOverdraft: c.AllowOverdraft,
		})
	}
	return out
}

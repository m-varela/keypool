package metrics

import _ "embed"

//go:embed dashboard.html
var dashboardHTML []byte

// DashboardHTML retorna a pagina estatica embutida no executavel.
func DashboardHTML() []byte { return append([]byte(nil), dashboardHTML...) }

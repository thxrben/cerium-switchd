package daemon

import (
	"github.com/thxrben/cerium-switchd/lib/conf/config"
	"github.com/thxrben/cerium-switchd/lib/conf/model"
)

// CheckConfig reads a stored configuration (JSON) the way this version
// reads it at startup, conversions of older versions included, and checks
// it (reference 3.6: the configuration check of a new version before an
// update). It returns the issues; ok is false if there are errors.
func CheckConfig(raw []byte) (issues string, ok bool) {
	up := newUpgrader(nil, 1, nil).Upgrade(raw)
	tr, err := config.FromJSON(up)
	if err != nil {
		return "error: " + err.Error() + "\n", false
	}
	_, is := model.Build(tr.Active(), nil)
	return is.String(), !is.HasErrors()
}

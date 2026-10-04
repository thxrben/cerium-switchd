package cli

import (
	"errors"
	"fmt"
	"time"
)

// WebStatus is show system services web-management (reference 3.5).
type WebStatus struct {
	Configured  bool
	Running     bool
	Member      int
	Port        int
	VRF         string
	Certificate string
	Generated   time.Time
	Fingerprint string
	Pin         string
	Error       string
	// The uploaded bundle (Version "": none).
	Upload struct {
		Version, SHA256, User string
		Size                  int64
		Time                  time.Time
	}
}

// WebManagement is implemented by switches that run the REST API.
type WebManagement interface {
	WebManagement() (WebStatus, error)
}

func (sh *Shell) showWebManagement(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	w, ok := sh.env.Ops.(WebManagement)
	if sh.env.Ops == nil || !ok {
		return errors.New("web-management information is not available")
	}
	st, err := w.WebManagement()
	if err != nil {
		return err
	}
	line := func(name, value string) { fmt.Fprintf(c.out, "  %-20s %s\n", name+":", value) }
	switch {
	case !st.Configured:
		c.out.WriteString("REST API: not configured (system services web-management)\n")
	case st.Running:
		fmt.Fprintf(c.out, "REST API: running on member %d, port %d, management instance %s\n", st.Member, st.Port, st.VRF)
	case st.Error != "":
		fmt.Fprintf(c.out, "REST API: not running: %s\n", st.Error)
	default:
		c.out.WriteString("REST API: not running (disabled, or no management instance)\n")
	}
	if st.Running {
		cert := st.Certificate
		if !st.Generated.IsZero() {
			cert += ", generated " + st.Generated.Format("2006-01-02 15:04:05 MST")
		}
		line("Certificate", cert)
		line("SHA-256 fingerprint", st.Fingerprint)
		line("Public-key pin", st.Pin+"  (curl --pinnedpubkey '"+st.Pin+"')")
	}
	if st.Upload.Version != "" {
		line("Uploaded bundle", fmt.Sprintf("%s, %d MB, by %s at %s", st.Upload.Version, (st.Upload.Size+(1<<20)-1)>>20,
			st.Upload.User, st.Upload.Time.Format("2006-01-02 15:04:05 MST")))
		line("Bundle SHA-256", st.Upload.SHA256)
	} else if st.Configured {
		line("Uploaded bundle", "none")
	}
	return nil
}

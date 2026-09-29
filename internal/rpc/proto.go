// Package rpc connects swcli to switchd: JSON lines over a unix socket.
// The server authenticates the peer by its kernel credentials
// (SO_PEERCRED); nothing the client sends identifies the user.
//
// Client to server:
//
//	{"t":"exec","line":…}      run a command line (empty: refresh prompt/banner)
//	{"t":"complete","line":…}  completions for the text before the cursor
//	{"t":"help","line":…}      "?" output
//	{"t":"answer","text":…}    reply to "ask" / "readtext" (err set on EOF)
//	{"t":"file","data":…}      reply to "readfile" / "writefile"
//	{"t":"interrupt"}          Ctrl-C during a running command
//
// Server to client:
//
//	{"t":"hello",…}            after connecting: prompt, banner, class (name)
//	{"t":"ask","prompt":…,"echo":…}
//	{"t":"readtext","prompt":…}
//	{"t":"readfile","name":…} / {"t":"writefile","name":…,"data":…}
//	{"t":"done","text":…,…}   result of exec, with the next prompt
//	{"t":"completions",…}     result of complete / help
//	{"t":"notify","text":…}   asynchronous message (e.g. another user's
//	                          commit), with the session's current prompt
//	                          and banner
package rpc

// Msg is one protocol message; unused fields are omitted.
type Msg struct {
	T      string `json:"t"`
	Line   string `json:"line,omitempty"`
	Text   string `json:"text,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	Banner string `json:"banner,omitempty"`
	Echo   bool   `json:"echo,omitempty"`
	Name   string `json:"name,omitempty"`
	Data   []byte `json:"data,omitempty"`
	Err    string `json:"err,omitempty"`
	NoMore bool   `json:"nomore,omitempty"`
	Exit   bool   `json:"exit,omitempty"`
	Shell  bool   `json:"shell,omitempty"`
	Items  []Item `json:"items,omitempty"`
}

// Item is one completion.
type Item struct {
	Text        string `json:"text"`
	Help        string `json:"help,omitempty"`
	Placeholder bool   `json:"ph,omitempty"`
}

// MaxMsg bounds the size of one message (a loaded configuration file).
const MaxMsg = 16 << 20

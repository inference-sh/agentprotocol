package agentprotocol

// TurnInput is what a user sends into a turn: the text and the files
// attached to it. It is the one shape for a prompt from the server that
// stores the message, through the host that runs the agent, to the driver
// that hands it to the agent, so none of them can drop a part the others
// carry.
//
// A file's URI is where its bytes are: a URL the server stores it at, or a
// file:// path on the host the agent runs on. A host fetches remote files
// before the driver sees them where it can, because a driver only reads local
// bytes: it sends an image the agent can take as an image, and names every
// other file, by path, in the text.
type TurnInput struct {
	Text  string    `json:"text"`
	Files []FileRef `json:"files,omitempty"`
}

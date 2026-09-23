// Package registrar imports the SQLite session codecs for their
// registration side effect, so a program can enable goose and hermes with a
// single blank import. It lives under the sqlite module, so importing it
// pulls in the database driver; a program that must stay driver-free imports
// transcript/all alone and gets the pure-Go codecs only.
package registrar

import _ "github.com/inference-sh/agentprotocol/transcript/sqlite"

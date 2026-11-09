// Package ui is the local web UI of repoctl: the server of `repoctl ui`.
//
// The server serves one user on one host, on a loopback address
// (localweb.Listen and localweb.PeerListener refuse everyone else). A
// browser gets in with a one-use launch token, which the server trades for
// a session cookie. Pages are rendered on the server from html/template
// blocks; htmx swaps fragments, and one Server-Sent Events stream per tab
// pushes the fragments that change.
//
// The data of a page comes from the repoctl commands themselves, run with
// --json for the directory of the set (Source), and from the run journals,
// which the server tails for live updates (watch.go, runfold.go). The UI
// changes nothing by itself: every action runs repoctl as a child process,
// and a publish passes its typed confirmation on file descriptor 3.
package ui

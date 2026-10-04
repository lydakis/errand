// Package nowindow keeps helper programs the runner starts from opening
// console windows.
//
// A Windows runner has no console of its own, so Windows gives every console
// program it starts a new, visible console window that flashes on the
// desktop. Elsewhere Hide does nothing.
package nowindow

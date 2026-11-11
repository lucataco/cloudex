package main

// The agent runs on Linux; on Windows only the build has to work.
func alive(pid int) bool { return pid > 0 }

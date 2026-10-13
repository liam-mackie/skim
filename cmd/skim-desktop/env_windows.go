package main

// Windows GUI processes inherit the user's full environment from the registry,
// so there is no login-shell environment to import.

const envProbeArg = "--skim-print-environment"

func printEnvironment() {}

func importLoginShellEnvironment() {}

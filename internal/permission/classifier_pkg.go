package permission

// Rating of build tools, package managers and docker for the classifier
// (see classifyWords).

// classifyBuild rates build/test tools by their subcommand (words[1]). run,
// generate and install execute or place arbitrary code and are CatUnknown.
func (c *Classifier) classifyBuild(name string, args []string) CmdCategory {
	if len(args) == 0 {
		return CatUnknown
	}
	sub := args[0]
	if name == "go" {
		switch sub {
		case "version":
			return CatSafe
		case "env":
			if hasAny(args[1:], "-w", "-u") {
				return CatUnknown
			}
			return CatSafe
		}
	}
	switch sub {
	case "build", "test", "bench", "compile", "lint", "vet", "fmt",
		"check", "clippy", "doc", "init", "mod":
		return CatBuild
	}
	return CatUnknown
}

// classifyPackageManager rates a package manager by its subcommand
// (words[1]). Only listing/inspecting subcommands are CatSafe; --version and
// --help only when they are the sole argument. Everything else, publish and
// cache clean included, is CatInstall and asks.
func (c *Classifier) classifyPackageManager(name string, args []string) CmdCategory {
	if len(args) == 0 {
		return CatInstall
	}
	sub := args[0]
	if len(args) == 1 && (sub == "--version" || sub == "--help") {
		return CatSafe
	}
	switch sub {
	case "list", "ls", "ll", "la", "info", "show", "view", "search", "outdated", "why", "explain", "freeze", "doctor":
		return CatSafe
	case "audit":
		if hasAny(args[1:], "fix") {
			return CatInstall
		}
		return CatSafe
	case "config":
		if len(args) > 1 && (args[1] == "list" || args[1] == "get") {
			return CatSafe
		}
	}
	return CatInstall
}

// classifyDocker allows only inspecting subcommands, matched exactly. A
// global option in front of the subcommand (-H, --context ...) is CatUnknown.
func (c *Classifier) classifyDocker(args []string) CmdCategory {
	if len(args) == 0 {
		return CatUnknown
	}
	switch args[0] {
	case "ps", "images", "inspect", "logs", "stats", "info", "version":
		return CatSafe
	}
	if len(args) >= 2 {
		switch args[0] + " " + args[1] {
		case "network ls", "network inspect", "volume ls", "volume inspect",
			"compose ps", "compose logs", "compose config", "context ls",
			"system info", "system df", "image ls", "image inspect",
			"container ls", "container inspect", "container logs":
			return CatSafe
		}
	}
	return CatUnknown
}

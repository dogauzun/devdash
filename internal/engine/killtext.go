package engine

// Mode names o as `kill N`'s plan and the dashboard's kill modal show it: "process mode",
// "tree mode", with ", force" after either.
func (o KillOptions) Mode() string {
	mode := "process mode"
	if o.Tree {
		mode = "tree mode"
	}
	if o.Force {
		mode += ", force"
	}
	return mode
}

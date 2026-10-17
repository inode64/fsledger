package event

// Labels of the actor text. The daemon writes the actor of each change with them, as commit text
// that the catalog also keeps as evidence, and catalog views parse the same labels back into
// display lines; sharing them keeps the writer and the reader in step.
const (
	ActorChangesDetected = "filesystem changes detected"
	ActorUnknownLine     = "Actor: " + Unknown
	ActorDetection       = "Detection:"
	ActorBackend         = "backend:"
	ActorReason          = "Reason:"
	ActorStartTime       = "start_time:"
	ActorExecutable      = "executable:"
)

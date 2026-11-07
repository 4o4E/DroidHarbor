package capture

// Settings controls the server's bounded video encoder configuration.
type Settings struct {
	MaxSize  int
	MaxFPS   int
	BitRate  int
	Revision int64
}

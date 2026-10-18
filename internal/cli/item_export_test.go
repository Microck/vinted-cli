package cli

// StubOpenBrowser records the URLs `item open` would hand to the desktop
// until the returned func restores the real opener.
func StubOpenBrowser(opened *[]string) (restore func()) {
	prev := openBrowser
	openBrowser = func(url string) error {
		*opened = append(*opened, url)
		return nil
	}
	return func() { openBrowser = prev }
}

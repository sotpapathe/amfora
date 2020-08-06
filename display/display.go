package display

import (
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/gdamore/tcell"
	"github.com/makeworld-the-better-one/amfora/cache"
	"github.com/makeworld-the-better-one/amfora/config"
	"github.com/makeworld-the-better-one/amfora/renderer"
	"github.com/makeworld-the-better-one/amfora/structs"
	"github.com/spf13/viper"
	"gitlab.com/tslocum/cview"
)

var tabs []*tab // Slice of all the current browser tabs
var curTab = -1 // What tab is currently visible - index for the tabs slice (-1 means there are no tabs)

// Terminal dimensions
var termW int
var termH int

// The user input and URL display bar at the bottom
var bottomBar = cview.NewInputField()

// Viewer for the tab primitives
// Pages are named as strings of tab numbers - so the textview for the first tab
// is held in the page named "0".
// The only pages that don't confine to this scheme are those named after modals,
// which are used to draw modals on top the current tab.
// Ex: "info", "error", "input", "yesno"
var tabPages = cview.NewPages()

// The tabs at the top with titles
var tabRow = cview.NewTextView().
	SetDynamicColors(true).
	SetRegions(true).
	SetScrollable(true).
	SetWrap(false).
	SetHighlightedFunc(func(added, removed, remaining []string) {
		// There will always only be one string in added - never multiple highlights
		// Remaining should always be empty
		i, _ := strconv.Atoi(added[0])
		tabPages.SwitchToPage(strconv.Itoa(i)) // Tab names are just numbers, zero-indexed
	})

// Root layout
var layout = cview.NewFlex().
	SetDirection(cview.FlexRow)

var renderedNewTabContent string
var newTabLinks []string
var newTabPage structs.Page

var App = cview.NewApplication().
	EnableMouse(false).
	SetRoot(layout, true).
	SetAfterResizeFunc(func(width int, height int) {
		// Store for calculations
		termW = width
		termH = height

		// Make sure the current tab content is reformatted when the terminal size changes
		go func(t *tab) {
			t.reformatMu.Lock() // Only one reformat job per tab
			defer t.reformatMu.Unlock()
			// Use the current tab, but don't affect other tabs if the user switches tabs
			reformatPageAndSetView(t, t.page)
		}(tabs[curTab])
	})

func Init() {
	tabRow.SetChangedFunc(func() {
		App.Draw()
	})

	helpInit()

	layout.
		AddItem(tabRow, 1, 1, false).
		AddItem(nil, 1, 1, false). // One line of empty space above the page
		AddItem(tabPages, 0, 1, true).
		AddItem(nil, 1, 1, false). // One line of empty space before bottomBar
		AddItem(bottomBar, 1, 1, false)

	if viper.GetBool("a-general.color") {
		layout.SetBackgroundColor(config.GetColor("bg"))
		tabRow.SetBackgroundColor(config.GetColor("bg"))

		bottomBar.SetBackgroundColor(config.GetColor("bottombar_bg"))
		bottomBar.
			SetLabelColor(config.GetColor("bottombar_label")).
			SetFieldBackgroundColor(config.GetColor("bottombar_bg")).
			SetFieldTextColor(config.GetColor("bottombar_text"))
	} else {
		bottomBar.SetBackgroundColor(tcell.ColorWhite)
		bottomBar.
			SetLabelColor(tcell.ColorBlack).
			SetFieldBackgroundColor(tcell.ColorWhite).
			SetFieldTextColor(tcell.ColorBlack)
	}
	bottomBar.SetDoneFunc(func(key tcell.Key) {
		tab := curTab

		tabs[tab].saveScroll()

		// Reset func to set the bottomBar back to what it was before
		// Use for errors.
		reset := func() {
			bottomBar.SetLabel("")
			tabs[tab].applyAll()
			App.SetFocus(tabs[tab].view)
		}

		switch key {
		case tcell.KeyEnter:
			// Figure out whether it's a URL, link number, or search
			// And send out a request

			query := bottomBar.GetText()

			if strings.TrimSpace(query) == "" {
				// Ignore
				reset()
				return
			}
			if query == ".." && tabs[tab].hasContent() {
				// Go up a directory
				parsed, err := url.Parse(tabs[tab].page.Url)
				if err != nil {
					// This shouldn't occur
					return
				}
				if parsed.Path == "/" {
					// Can't go up further
					reset()
					return
				}

				// Ex: /test/foo/ -> /test/foo//.. -> /test -> /test/
				parsed.Path = path.Clean(parsed.Path+"/..") + "/"
				if parsed.Path == "//" {
					// Fix double slash that occurs at domain root
					parsed.Path = "/"
				}
				parsed.RawQuery = "" // Remove query
				URL(parsed.String())
				return
			}

			i, err := strconv.Atoi(query)
			if err != nil {
				if strings.HasPrefix(query, "new:") && len(query) > 4 {
					// They're trying to open a link number in a new tab
					i, err = strconv.Atoi(query[4:])
					if err != nil {
						reset()
						return
					}
					if i <= len(tabs[tab].page.Links) && i > 0 {
						// Open new tab and load link
						oldTab := tab
						NewTab()
						// Resolve and follow link manually
						prevParsed, _ := url.Parse(tabs[oldTab].page.Url)
						nextParsed, err := url.Parse(tabs[oldTab].page.Links[i-1])
						if err != nil {
							Error("URL Error", "link URL could not be parsed")
							reset()
							return
						}
						URL(prevParsed.ResolveReference(nextParsed).String())
						return
					}
				} else {
					// It's a full URL or search term
					// Detect if it's a search or URL
					if strings.Contains(query, " ") || (!strings.Contains(query, "//") && !strings.Contains(query, ".") && !strings.HasPrefix(query, "about:")) {
						u := viper.GetString("a-general.search") + "?" + queryEscape(query)
						cache.RemovePage(u) // Don't use the cached version of the search
						URL(u)
					} else {
						// Full URL
						cache.RemovePage(query) // Don't use cached version for manually entered URL
						URL(query)
					}
					return
				}
			}
			if i <= len(tabs[tab].page.Links) && i > 0 {
				// It's a valid link number
				followLink(tabs[tab], tabs[tab].page.Url, tabs[tab].page.Links[i-1])
				return
			}
			// Invalid link number, don't do anything
			reset()
			return

		case tcell.KeyEsc:
			// Set back to what it was
			reset()
			return
		}
		// Other potential keys are Tab and Backtab, they are ignored
	})

	// Render the default new tab content ONCE and store it for later
	renderedNewTabContent, newTabLinks = renderer.RenderGemini(newTabContent, textWidth(), leftMargin())
	newTabPage = structs.Page{
		Raw:       newTabContent,
		Content:   renderedNewTabContent,
		Links:     newTabLinks,
		Url:       "about:newtab",
		Width:     -1, // Force reformatting on first display
		Mediatype: structs.TextGemini,
	}

	modalInit()

	// Setup map of keys to functions here
	// Changing tabs, new tab, etc
	App.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		_, ok := App.GetFocus().(*cview.Button)
		if ok {
			// It's focused on a modal right now, nothing should interrupt
			return event
		}
		_, ok = App.GetFocus().(*cview.InputField)
		if ok {
			// An InputField is in focus, nothing should interrupt
			return event
		}
		_, ok = App.GetFocus().(*cview.Modal)
		if ok {
			// It's focused on a modal right now, nothing should interrupt
			return event
		}

		if tabs[curTab].mode == tabModeDone {
			// All the keys and operations that can only work while NOT loading

			// History arrow keys
			if event.Modifiers() == tcell.ModAlt {
				if event.Key() == tcell.KeyLeft {
					histBack(tabs[curTab])
					return nil
				}
				if event.Key() == tcell.KeyRight {
					histForward(tabs[curTab])
					return nil
				}
			}

			switch event.Key() {
			case tcell.KeyCtrlR:
				Reload()
				return nil
			case tcell.KeyCtrlH:
				URL(viper.GetString("a-general.home"))
				return nil
			case tcell.KeyCtrlB:
				Bookmarks(tabs[curTab])
				tabs[curTab].addToHistory("about:bookmarks")
				return nil
			case tcell.KeyCtrlD:
				go addBookmark()
				return nil
			case tcell.KeyPgUp:
				tabs[curTab].pageUp()
				return nil
			case tcell.KeyPgDn:
				tabs[curTab].pageDown()
				return nil
			case tcell.KeyCtrlS:
				if tabs[curTab].hasContent() {
					savePath, err := downloadPage(tabs[curTab].page)
					if err != nil {
						Error("Download Error", fmt.Sprintf("Error saving page content: %v", err))
					} else {
						Info(fmt.Sprintf("Page content saved to %s. ", savePath))
					}
				} else {
					Info("The current page has no content, so it couldn't be downloaded.")
				}
				return nil
			case tcell.KeyRune:
				// Regular key was sent
				switch string(event.Rune()) {
				case " ":
					// Space starts typing, like Bombadillo
					bottomBar.SetLabel("[::b]URL/Num./Search: [::-]")
					bottomBar.SetText("")
					// Don't save bottom bar, so that whenever you switch tabs, it's not in that mode
					App.SetFocus(bottomBar)
					return nil
				case "R":
					Reload()
					return nil
				case "b":
					histBack(tabs[curTab])
					return nil
				case "f":
					histForward(tabs[curTab])
					return nil
				case "u":
					tabs[curTab].pageUp()
					return nil
				case "d":
					tabs[curTab].pageDown()
					return nil
				}

				// Number key: 1-9, 0
				i, err := strconv.Atoi(string(event.Rune()))
				if err == nil {
					if i == 0 {
						i = 10 // 0 key is for link 10
					}
					if i <= len(tabs[curTab].page.Links) && i > 0 {
						// It's a valid link number
						followLink(tabs[curTab], tabs[curTab].page.Url, tabs[curTab].page.Links[i-1])
						return nil
					}
				}
			}
		}
		// All the keys and operations that can work while a tab IS loading

		switch event.Key() {
		case tcell.KeyCtrlT:
			if tabs[curTab].page.Mode == structs.ModeLinkSelect {
				next, err := resolveRelLink(tabs[curTab], tabs[curTab].page.Url, tabs[curTab].page.Selected)
				if err != nil {
					Error("URL Error", err.Error())
					return nil
				}
				NewTab()
				URL(next)
			} else {
				NewTab()
			}
			return nil
		case tcell.KeyCtrlW:
			CloseTab()
			return nil
		case tcell.KeyCtrlQ:
			Stop()
			return nil
		case tcell.KeyCtrlC:
			Stop()
			return nil
		case tcell.KeyRune:
			// Regular key was sent

			if num, err := config.KeyToNum(event.Rune()); err == nil {
				// It's a Shift+Num key
				if num == 0 {
					// Zero key goes to the last tab
					SwitchTab(NumTabs() - 1)
				} else {
					SwitchTab(num - 1)
				}
				return nil
			}

			switch string(event.Rune()) {
			case "q":
				Stop()
				return nil
			case "?":
				Help()
				return nil
			}
		}

		// Let another element handle the event, it's not a special global key
		return event
	})
}

// Stop stops the app gracefully.
// In the future it will handle things like ongoing downloads, etc
func Stop() {
	App.Stop()
}

// NewTab opens a new tab and switches to it, displaying the
// the default empty content because there's no URL.
func NewTab() {
	// Create TextView and change curTab
	// Set the TextView options, and the changed func to App.Draw()
	// SetDoneFunc to do link highlighting
	// Add view to pages and switch to it

	// Process current tab before making a new one
	if curTab > -1 {
		// Turn off link selecting mode in the current tab
		tabs[curTab].view.Highlight("")
		// Save bottomBar state
		tabs[curTab].saveBottomBar()
		tabs[curTab].saveScroll()
	}

	curTab = NumTabs()

	tabs = append(tabs, makeNewTab())
	temp := newTabPage // Copy
	setPage(tabs[curTab], &temp)

	// Can't go backwards, but this isn't the first page either.
	// The first page will be the next one the user goes to.
	tabs[curTab].history.pos = -1

	tabPages.AddAndSwitchToPage(strconv.Itoa(curTab), tabs[curTab].view, true)
	App.SetFocus(tabs[curTab].view)

	// Add tab number to the actual place where tabs are show on the screen
	// Tab regions are 0-indexed but text displayed on the screen starts at 1
	if viper.GetBool("a-general.color") {
		fmt.Fprintf(tabRow, `["%d"][%s]  %d  [%s][""]|`,
			curTab,
			config.GetColorString("tab_num"),
			curTab+1,
			config.GetColorString("tab_divider"),
		)
	} else {
		fmt.Fprintf(tabRow, `["%d"]  %d  [""]|`, curTab, curTab+1)
	}
	tabRow.Highlight(strconv.Itoa(curTab)).ScrollToHighlight()

	bottomBar.SetLabel("")
	bottomBar.SetText("")
	tabs[curTab].saveBottomBar()

	// Draw just in case
	App.Draw()
}

// CloseTab closes the current tab and switches to the one to its left.
func CloseTab() {
	// Basically the NewTab() func inverted

	// TODO: Support closing middle tabs, by renumbering all the maps
	// So that tabs to the right of the closed tabs point to the right places
	// For now you can only close the right-most tab
	if curTab != NumTabs()-1 {
		return
	}

	if NumTabs() <= 1 {
		// There's only one tab open, close the app instead
		Stop()
		return
	}

	tabs = tabs[:len(tabs)-1]
	tabPages.RemovePage(strconv.Itoa(curTab))

	if curTab <= 0 {
		curTab = NumTabs() - 1
	} else {
		curTab--
	}

	tabPages.SwitchToPage(strconv.Itoa(curTab)) // Go to previous page
	rewriteTabRow()
	// Restore previous tab's state
	tabs[curTab].applyAll()

	App.SetFocus(tabs[curTab].view)

	// Just in case
	App.Draw()
}

// SwitchTab switches to a specific tab, using its number, 0-indexed.
// The tab numbers are clamped to the end, so for example numbers like -5 and 1000 are still valid.
// This means that calling something like SwitchTab(curTab - 1) will never cause an error.
func SwitchTab(tab int) {
	if tab < 0 {
		tab = 0
	}
	if tab > NumTabs()-1 {
		tab = NumTabs() - 1
	}

	// Save current tab attributes
	if curTab > -1 {
		// Save bottomBar state
		tabs[curTab].saveBottomBar()
		tabs[curTab].saveScroll()
	}

	curTab = tab % NumTabs()

	// Display tab
	reformatPageAndSetView(tabs[curTab], tabs[curTab].page)
	tabPages.SwitchToPage(strconv.Itoa(curTab))
	tabRow.Highlight(strconv.Itoa(curTab)).ScrollToHighlight()
	tabs[curTab].applyAll()

	App.SetFocus(tabs[curTab].view)

	// Just in case
	App.Draw()
}

func Reload() {
	if !tabs[curTab].hasContent() {
		return
	}

	parsed, _ := url.Parse(tabs[curTab].page.Url)
	go func(t *tab) {
		cache.RemovePage(tabs[curTab].page.Url)
		cache.RemoveFavicon(parsed.Host)
		handleURL(t, t.page.Url) // goURL is not used bc history shouldn't be added to
		if t == tabs[curTab] {
			// Display the bottomBar state that handleURL set
			t.applyBottomBar()
		}
	}(tabs[curTab])
}

// URL loads and handles the provided URL for the current tab.
// It should be an absolute URL.
func URL(u string) {
	// Some code is copied in followLink()

	if u == "about:bookmarks" {
		Bookmarks(tabs[curTab])
		tabs[curTab].addToHistory("about:bookmarks")
		return
	}
	if u == "about:newtab" {
		temp := newTabPage // Copy
		setPage(tabs[curTab], &temp)
		return
	}
	if strings.HasPrefix(u, "about:") {
		Error("Error", "Not a valid 'about:' URL.")
		return
	}

	go goURL(tabs[curTab], u)
}

func NumTabs() int {
	return len(tabs)
}

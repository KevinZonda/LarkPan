// LarkPan client GUI (MyGo native UI): login -> browse the remote tree ->
// download; plus a button to spawn `rclone mount` as a drive letter.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"larkpan/internal/client"
)

type entry struct {
	Name     string    `json:"name"`
	Type     string    `json:"type"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type state struct {
	cfg    *client.Config
	server string
	token  string
	dir    string
	ents   []entry
	status string
	hc     *http.Client
	rc     *exec.Cmd    // rclone mount child, if started
	win    *mygo.Window // nil while hidden in tray
	tray   *mygo.Tray
}

func (s *state) get(p string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, s.cfg.Server+p, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	return s.hc.Do(req)
}

func (s *state) refresh() {
	if s.cfg == nil || s.cfg.Server == "" {
		s.status = "not logged in"
		return
	}
	p := "/json/"
	if s.dir != "" {
		p += s.dir + "/"
	}
	resp, err := s.get(p)
	if err != nil {
		s.status = "error: " + err.Error()
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		s.status = "list: " + resp.Status
		return
	}
	var ents []entry
	if err := json.NewDecoder(resp.Body).Decode(&ents); err != nil {
		s.status = err.Error()
		return
	}
	s.ents = ents
	s.status = fmt.Sprintf("%d items", len(ents))
}

func (s *state) login() {
	s.cfg = &client.Config{Server: strings.TrimRight(s.server, "/"), Token: s.token}
	if err := s.cfg.Save(); err != nil {
		s.status = err.Error()
		return
	}
	s.dir = ""
	s.refresh()
}

func humanSize(b int64) string {
	for _, u := range []string{"B", "KB", "MB", "GB", "TB"} {
		if b < 1024 || u == "TB" {
			return fmt.Sprintf("%d %s", b, u)
		}
		b /= 1024
	}
	return ""
}

func (s *state) download(e entry) {
	rel := path.Join(s.dir, e.Name)
	resp, err := s.get("/files/" + rel)
	if err != nil || resp.StatusCode != 200 {
		s.status = "download: " + err.Error()
		return
	}
	defer resp.Body.Close()
	dst := filepath.Join(os.TempDir(), e.Name)
	f, err := os.Create(dst)
	if err != nil {
		s.status = err.Error()
		return
	}
	defer f.Close()
	io.Copy(f, resp.Body)
	s.status = "saved " + dst
}

func (s *state) mount() {
	conf := fmt.Sprintf("[larkpan]\ntype = webdav\nurl = %s/dav/\nvendor = other\nbearer_token = %s\n",
		s.cfg.Server, s.cfg.Token)
	tmp := filepath.Join(os.TempDir(), "larkpan-rclone.conf")
	os.WriteFile(tmp, []byte(conf), 0o600)
	cmd := exec.Command("rclone", "mount", "larkpan:", "Z:",
		"--vfs-cache-mode", "full", "--config", tmp)
	if err := cmd.Start(); err != nil {
		s.status = "rclone: " + err.Error()
		return
	}
	s.rc = cmd
	s.status = fmt.Sprintf("mounted Z: (rclone pid %d)", cmd.Process.Pid)
}

func (s *state) unmount() {
	if s.rc == nil {
		s.status = "not mounted"
		return
	}
	s.rc.Process.Kill()
	s.rc = nil
	s.status = "unmounted"
}

// cleanup kills the rclone child — call on window close / app quit.
func (s *state) cleanup() {
	if s.rc != nil {
		s.rc.Process.Kill()
		s.rc = nil
	}
}

// showWindow creates (or re-creates) the main window. Closing it keeps
// the app alive in the tray; Quit is only via the tray menu.
func (s *state) showWindow() {
	if s.win != nil {
		s.win.Show()
		s.win.Focus()
		return
	}
	w := mygo.NewWindow(mygo.WindowOptions{
		Title:   "LarkPan",
		Content: ui.View(s.view),
		Width:   640,
		Height:  480,
	})
	s.win = w
	w.SetIcon(appIcon())
	// 关闭 ≠ 退出: veto the close and hide to the tray instead. The
	// window object stays alive; tray Show reuses it. A real destroy
	// (OnClosed) only happens when the process is dying — and then the
	// mount must be released.
	w.OnClose(func(e *mygo.CloseEvent) {
		if s.tray != nil {
			e.PreventDefault()
			w.Hide()
		}
	})
	w.OnClosed(func() {
		s.win = nil
		s.cleanup()
		mygo.App.Quit()
	})
}

// makeTray installs the notification-area icon with Show/Quit items.
// Closing the window just hides the UI — the app lives on in the tray.
func (s *state) makeTray() {
	menu := mygo.NewMenu([]*mygo.MenuItem{
		{ID: "show", Label: "Show LarkPan", Click: func(*mygo.MenuItem, *mygo.Window) {
			s.showWindow()
		}},
		{Type: mygo.MenuItemSeparator},
		{ID: "quit", Label: "Quit", Click: func(*mygo.MenuItem, *mygo.Window) {
			s.cleanup()
			s.tray.Destroy()
			mygo.App.Quit()
		}},
	})
	t, err := mygo.NewTray(mygo.TrayOptions{
		ID:      "larkpan",
		Icon:    appIcon(),
		ToolTip: "LarkPan",
		Menu:    menu,
	})
	if err != nil {
		return // tray unsupported: closing the window quits as before
	}
	s.tray = t
	t.OnClick(s.showWindow)
}

func (s *state) view(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Fill().Padding(20).Gap(12).Children(func() {
		// header
		ui.Text(c, "LarkPan").FontSize(24).Bold()
		ui.Text(c, "Feishu Drive shared storage").TextColor(t.TextMuted)

		// login card
		ui.Box(c).Padding(12).Border(1, t.Border).Radius(8).Children(func() {
			ui.Column(c).Gap(8).Children(func() {
				ui.Field(c, "Server", func() {
					ui.TextInput(c, &s.server).Grow(1).Placeholder("http://edge:8080")
				})
				ui.Field(c, "Token", func() {
					ui.TextInput(c, &s.token).Grow(1).Placeholder("lp_... token")
				})
				ui.Row(c).Gap(8).Children(func() {
					if ui.PrimaryButton(c, "Login + Save").Clicked() {
						s.login()
					}
					ui.Spacer(c)
					if s.rc == nil {
						if ui.Button(c, "Mount Z:").Clicked() {
							s.mount()
						}
					} else if ui.Button(c, "Unmount").Clicked() {
						s.unmount()
					}
				})
			})
		})

		ui.Divider(c)

		// toolbar
		ui.Row(c).Gap(8).Children(func() {
			if ui.Button(c, "↑ Up").Clicked() {
				s.dir = strings.Trim(path.Dir(s.dir), "/")
				if s.dir == "." {
					s.dir = ""
				}
				s.refresh()
			}
			ui.Text(c, "/"+s.dir).TextColor(t.TextMuted)
			ui.Spacer(c)
			if ui.Button(c, "Refresh").Clicked() {
				s.refresh()
			}
		})

		// file list
		ui.Box(c).Fill().Padding(4).Border(1, t.Border).Radius(8).Children(func() {
			ui.Scroll(c).Fill().Children(func() {
				ui.Column(c).Gap(2).Children(func() {
					for i := range s.ents {
						e := s.ents[i]
						var name, right string
						if e.Type == "dir" {
							name, right = e.Name+"/", "dir"
						} else {
							name, right = e.Name, humanSize(e.Size)
						}
						row := ui.ButtonBase(c).Fill()
						row.Children(func() {
							ui.Row(c).Gap(8).Children(func() {
								ui.Text(c, name)
								ui.Spacer(c)
								if right != "" {
									ui.Text(c, right).TextColor(t.TextMuted)
								}
							})
						})
						if row.Clicked() {
							if e.Type == "dir" {
								s.dir = path.Join(s.dir, e.Name)
								s.refresh()
							} else {
								s.download(e)
							}
							return
						}
					}
					if len(s.ents) == 0 {
						ui.Text(c, "empty").TextColor(t.TextMuted)
					}
				})
			})
		})

		ui.Text(c, s.status).TextColor(t.TextMuted).Wrap()
	})
}

// appIcon renders a small PNG at runtime (no asset needed) for the
// window/taskbar icon: rounded blue square with a white "L".
func appIcon() []byte {
	const n = 64
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	blue := color.RGBA{0x2f, 0x6f, 0xde, 0xff}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if x > 6 && x < n-6 && y > 6 && y < n-6 {
				img.Set(x, y, blue)
			}
		}
	}
	for y := 14; y < 50; y++ {
		for x := 22; x < 28; x++ {
			img.Set(x, y, color.White)
		}
	}
	for y := 44; y < 50; y++ {
		for x := 22; x < 44; x++ {
			img.Set(x, y, color.White)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func main() {
	s := &state{hc: &http.Client{Timeout: 60 * time.Second}}
	if cfg, err := client.Load(); err == nil {
		s.cfg, s.server, s.token = cfg, cfg.Server, cfg.Token
		s.refresh()
	} else {
		s.status = "not logged in"
	}
	mygo.App.WhenReady(func() {
		s.showWindow()
		s.makeTray()
	})
	if err := mygo.App.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

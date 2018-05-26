package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"io"
	"io/ioutil"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/mattn/go-gtk/gdk"
	"github.com/mattn/go-gtk/gdkpixbuf"
	"github.com/mattn/go-gtk/glib"

	"github.com/mattn/go-gtk/gtk"

	"github.com/nwaples/rardecode"
)

var (
	dev       = flag.Bool("dev", false, "Print error messages")
	dump      = flag.Bool("dump", false, "Dump the archive for dev")
	index     int
	window    *gtk.Window
	auto      chan int
	pages     []*Page
	pagesLock = &sync.RWMutex{}
	img       *gtk.Image
)

func PopError(msg string) {
	fmt.Println(msg)
}

func main() {
	flag.Parse()

	gtk.Init(&os.Args)
	window = gtk.NewWindow(gtk.WINDOW_TOPLEVEL)
	window.SetTitle("Comic View")
	window.Connect("destroy", gtk.MainQuit)

	window.SetResizable(true)
	f, err := os.Open(os.Getenv("HOME") + "/.comicview")
	if err == nil {
		c := Config{}
		if json.NewDecoder(f).Decode(&c) == nil {
			window.SetSizeRequest(c.W, c.H)
			window.Move(c.X, c.Y)
		}
	}
	img = gtk.NewImage()
	window.Add(img)
	DrawCurrent()

	SetupEvents()
	window.ShowAll()

	auto = make(chan int)

	go func() {
		if len(flag.Args()) != 0 {
			OpenFile(flag.Args()[0])
		}
	}()
	go func() {
		running := false
		t := time.NewTicker(4 * time.Second)
		for {
			select {
			case s := <-auto:
				running = s == 1
			case <-t.C:
				if running {
					NextPage()
				}
			}
		}
	}()
	gtk.Main()
}
func OpenFile(filename string) {
	pagesLock.Lock()
	pages = []*Page{}
	pagesLock.Unlock()
	index = 0
	filename = strings.TrimSpace(filename)
	switch path.Ext(filename) {
	case ".zip", ".cbz":
		go ReadZip(filename)
	case ".rar", ".cbr":
		go ReadRar(filename)
	default:
		fmt.Printf("Unknown extension: '%s'\n", path.Ext(filename))
		fmt.Println([]byte(path.Ext(filename)))
		return
	}
	DrawCurrent()
}
func SetupEvents() {
	event := make(chan interface{})
	window.Connect("key-press-event", func(ctx *glib.CallbackContext) {
		arg := ctx.Args(0)
		event <- *(**gdk.EventKey)(unsafe.Pointer(&arg))
	})
	window.Connect("scroll-event", func(ctx *glib.CallbackContext) {
		arg := ctx.Args(0)
		fmt.Println("scroll")
		event <- *(**gdk.EventScroll)(unsafe.Pointer(&arg))
	})
	window.Connect("configure-event", func(ctx *glib.CallbackContext) {
		SavePosition()
		DrawCurrent()
	})
	window.SetEvents(int(gdk.ALL_EVENTS_MASK))
	go WatchEvents(event)

	targets := []gtk.TargetEntry{
		{"text/uri-list", 0, 0},
		{"STRING", 0, 1},
		{"text/plain", 0, 2},
	}
	img.DragDestSet(
		gtk.DEST_DEFAULT_MOTION|
			gtk.DEST_DEFAULT_HIGHLIGHT|
			gtk.DEST_DEFAULT_DROP,
		targets,
		gdk.ACTION_COPY,
	)
	img.DragDestAddUriTargets()
	img.Connect("drag-data-received", func(ctx *glib.CallbackContext) {
		sdata := gtk.NewSelectionDataFromNative(unsafe.Pointer(ctx.Args(3)))
		if sdata != nil {
			a := (*[2000]uint8)(sdata.GetData())
			files := strings.Split(string(a[0:sdata.GetLength()-1]), "\n")
			for i := range files {
				filename, _, _ := glib.FilenameFromUri(files[i])
				files[i] = filename
			}
			OpenFile(files[0])
		}
	})
}
func NextPage() {
	pagesLock.RLock()
	if index < len(pages) {
		index++
	}
	pagesLock.RUnlock()
	DrawCurrent()
}
func PrevPage() {
	if index > 0 {
		index--
	}
	DrawCurrent()
}
func DrawCurrent() {
	count := 0
	pagesLock.RLock()
	count = len(pages)
	pagesLock.RUnlock()
	if count == 0 {
		return
	}

	pb, err := gdkpixbuf.NewPixbufFromBytes(pages[index].Buf)
	if err != nil {
		fmt.Println("New Pixbuf: ", err)
		return
	}
	w, h := window.GetSize()
	img.SetFromPixbuf(Resize(pb, w, h))
	window.SetTitle(fmt.Sprintf("ComicView - %d of %d", index, count))
}

func WatchEvents(event chan interface{}) {
	for {
		e := <-event
		switch ev := e.(type) {
		case *gdk.EventKey:
			fmt.Println("key-press-event:", ev.Keyval)
			switch ev.Keyval {
			case gdk.KEY_Right, gdk.KEY_space:
				NextPage()
			case gdk.KEY_Left:
				PrevPage()
			case gdk.KEY_S, gdk.KEY_s:
				auto <- 1
			case gdk.KEY_A, gdk.KEY_a:
				auto <- 0
			}
		case *gdk.EventScroll:
			if ev.State == 16 {
				PrevPage()
			} else {
				NextPage()
			}
		default:
			DrawCurrent()
		}
	}
}

type Config struct {
	X, Y int
	W, H int
}

func SavePosition() {
	c := Config{}
	c.W, c.H = window.GetSize()
	c.X, c.Y = window.GetPosition()

	f, _ := os.Create(os.Getenv("HOME") + "/.comicview")
	b, _ := json.Marshal(c)
	fmt.Println(string(b))
	f.Write(b)
	f.Close()
}
func Resize(pb *gdkpixbuf.Pixbuf, width, height int) *gdkpixbuf.Pixbuf {
	pw, ph := pb.GetWidth(), pb.GetHeight()
	ps := float32(pw) / float32(ph)
	ws := float32(width) / float32(height)
	if ps <= ws {
		scale := float32(height) / float32(ph)
		nh := int(scale * float32(ph))
		nw := int(scale * float32(pw))
		return pb.ScaleSimple(nw, nh, gdkpixbuf.INTERP_BILINEAR)
	} else {
		scale := float32(width) / float32(pw)
		nh := int(scale * float32(ph))
		nw := int(scale * float32(pw))
		return pb.ScaleSimple(nw, nh, gdkpixbuf.INTERP_BILINEAR)
	}
}

type Page struct {
	Name string
	Buf  []byte
	Img  image.Image
}
type ps []*Page

func (p ps) Len() int {
	return len(p)
}
func (p ps) Less(i, j int) bool {
	return p[i].Name < p[j].Name
}
func (p ps) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}
func Sort() {
	pagesLock.Lock()
	sort.Sort(ps(pages))
	for _, p := range pages {
		fmt.Println(p.Name)
	}
	pagesLock.Unlock()

}
func ReadZip(filename string) {
	f, err := zip.OpenReader(filename)
	if err != nil {
		fmt.Println("Can't open file: ", filename)
		if *dev {
			fmt.Println(err)
		}
		return
	}
	window.SetTitle("Loading...")
	names := []string{}
	nameTo := map[string]*zip.File{}
	for _, zf := range f.File {
		if !zf.FileInfo().IsDir() {
			names = append(names, zf.Name)
			nameTo[zf.Name] = zf
		}
	}
	sort.Strings(names)
	for _, name := range names {
		zf := nameTo[name]
		if !zf.FileInfo().IsDir() {
			r, e := zf.Open()
			if e != nil {
				if *dev {
					fmt.Println("Open", zf.Name, err)
				}
			} else {
				e = DecodePage(zf.Name, r)
				if e != nil {
					fmt.Println("read", zf.Name, err)
				}
				r.Close()
			}
		}
	}
	DrawCurrent()
	f.Close()
}

func ReadRar(filename string) {
	f, err := rardecode.OpenReader(filename, "")
	if err != nil {
		fmt.Println("Can't open file: ", filename)
		if *dev {
			fmt.Println(err)
		}
		return
	}
	window.SetTitle("Loading...")
	rf, err := f.Next()
	for err == nil && rf != nil {
		fmt.Println(rf.Name)
		buf := &bytes.Buffer{}
		b := make([]byte, 100000)
		n, err := f.Read(b)
		for err == nil && n > 0 {
			buf.Write(b[:n])
			n, err = f.Read(b)
		}
		err = DecodePage(rf.Name, buf)
		if err != nil {
			if *dev {
				fmt.Println(err)
			}

		}

		rf, err = f.Next()
	}
	f.Close()
	Sort()
}

var validExtensions = map[string]bool{".png": true, ".jpeg": true, ".jpg": true, ".gif": true}

func DecodePage(name string, content io.Reader) error {
	if validExtensions[path.Ext(name)] {
		buf, _ := ioutil.ReadAll(content)
		if len(buf) == 0 {
			return errors.New("No content")
		}
		pagesLock.Lock()
		pages = append(pages, &Page{Name: name, Buf: buf})
		pagesLock.Unlock()
		return nil
	}
	return errors.New("Unrecognized page")
}

package torrent

import (
	"encoding/hex"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cenkalti/log"
	"github.com/cenkalti/rain/internal/logger"
	"github.com/cenkalti/rain/internal/metainfo"
	"github.com/cenkalti/rain/internal/storage/filestorage"
	"github.com/cenkalti/rain/internal/webseedsource"
	"github.com/fortytw2/leaktest"
)

var (
	torrentFile           = filepath.Join("testdata", "sample_torrent.torrent")
	torrentInfoHashString = "4242e334070406956b87c25f7c36251d32743461"
	torrentDataDir        = "testdata"
	torrentName           = "sample_torrent"
	timeout               = 10 * time.Second
)

func init() {
	logger.SetLevel(log.DEBUG)
}

func newFileStorage(t *testing.T, dir string) *filestorage.FileStorage {
	sto, err := filestorage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return sto
}

func TestDownloadMagnet(t *testing.T) {
	defer leaktest.Check(t)()

	where, err := ioutil.TempDir("", "rain-")
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(torrentFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	mi, err := metainfo.New(f)
	if err != nil {
		t.Fatal(err)
	}
	opt1 := options{
		Info: mi.Info,
	}
	t1, err := opt1.NewTorrent(mi.Info.Hash[:], newFileStorage(t, torrentDataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer t1.Close()

	opt2 := options{}
	ih, err := hex.DecodeString(torrentInfoHashString)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := opt2.NewTorrent(ih, newFileStorage(t, where))
	if err != nil {
		t.Fatal(err)
	}
	defer t2.Close()

	t1.Start()
	t2.Start()

	var port int
	select {
	case port = <-t1.NotifyListen():
	case err = <-t1.NotifyError():
		t.Fatal(err)
	case <-time.After(timeout):
		panic("seeder is not ready")
	}

	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	t2.AddPeers([]*net.TCPAddr{addr})

	select {
	case <-t2.NotifyComplete():
	case err = <-t2.NotifyError():
		t.Fatal(err)
	case <-time.After(timeout):
		panic("download did not finish")
	}

	cmd := exec.Command("diff", "-rq",
		filepath.Join(torrentDataDir, torrentName),
		filepath.Join(where, torrentName))
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	err = os.RemoveAll(where)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDownloadWebseed(t *testing.T) {
	defer leaktest.Check(t)()

	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	port := l.Addr().(*net.TCPAddr).Port
	servingDone := make(chan struct{})
	go func() {
		http.Serve(l, http.FileServer(http.Dir("./testdata")))
		close(servingDone)
	}()
	defer func() {
		l.Close()
		<-servingDone
	}()

	where, err := ioutil.TempDir("", "rain-")
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(torrentFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	mi, err := metainfo.New(f)
	if err != nil {
		t.Fatal(err)
	}

	opt2 := options{
		Info: mi.Info,
	}
	ih, err := hex.DecodeString(torrentInfoHashString)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := opt2.NewTorrent(ih, newFileStorage(t, where))
	if err != nil {
		t.Fatal(err)
	}
	defer t2.Close()

	t2.webseedSources = webseedsource.NewList([]string{"http://127.0.0.1:" + strconv.Itoa(port)})
	t2.webseedClient = http.DefaultClient
	t2.Start()

	select {
	case <-t2.NotifyComplete():
	case err = <-t2.NotifyError():
		t.Fatal(err)
	case <-time.After(timeout):
		panic("download did not finish")
	}

	cmd := exec.Command("diff", "-rq",
		filepath.Join(torrentDataDir, torrentName),
		filepath.Join(where, torrentName))
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	err = os.RemoveAll(where)
	if err != nil {
		t.Fatal(err)
	}
}

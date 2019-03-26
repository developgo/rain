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

func seeder(t *testing.T) (addr *net.TCPAddr, c func()) {
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
	t1.Start()
	var port int
	select {
	case port = <-t1.NotifyListen():
	case err = <-t1.NotifyError():
		t.Fatal(err)
	case <-time.After(timeout):
		t.Fatal("seeder is not ready")
	}
	addr = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	return addr, func() {
		t1.Close()
	}
}

func tempdir(t *testing.T) (string, func()) {
	where, err := ioutil.TempDir("", "rain-")
	if err != nil {
		t.Fatal(err)
	}
	return where, func() {
		err = os.RemoveAll(where)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestDownloadMagnet(t *testing.T) {
	defer leaktest.Check(t)()
	addr, cl := seeder(t)
	defer cl()
	where, clw := tempdir(t)
	defer clw()

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

	t2.Start()

	t2.AddPeers([]*net.TCPAddr{addr})

	assertCompleted(t, t2, where)
}

func webseed(t *testing.T) (port int, c func()) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = l.Addr().(*net.TCPAddr).Port
	servingDone := make(chan struct{})
	srv := &http.Server{Handler: http.FileServer(http.Dir("./testdata"))}
	go func() {
		srv.Serve(l)
		close(servingDone)
	}()
	return port, func() {
		srv.Close()
		l.Close()
		<-servingDone
	}

}

func TestDownloadWebseed(t *testing.T) {
	defer leaktest.Check(t)()
	port1, close1 := webseed(t)
	defer close1()
	port2, close2 := webseed(t)
	defer close2()
	where, clw := tempdir(t)
	defer clw()

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

	t2.webseedSources = webseedsource.NewList([]string{
		"http://127.0.0.1:" + strconv.Itoa(port1),
		"http://127.0.0.1:" + strconv.Itoa(port2),
	})
	t2.webseedClient = http.DefaultClient
	t2.Start()

	assertCompleted(t, t2, where)
}

func assertCompleted(t *testing.T, t2 *torrent, where string) {
	select {
	case <-t2.NotifyComplete():
	case err := <-t2.NotifyError():
		t.Fatal(err)
	case <-time.After(timeout):
		t.Fatal("download did not finish")
	}

	cmd := exec.Command("diff", "-rq",
		filepath.Join(torrentDataDir, torrentName),
		filepath.Join(where, torrentName))
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

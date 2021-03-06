package common

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const (
	// TransferGet get file from remote servers
	TransferGet = "GET"
	// TransferPut put file to remote servers
	TransferPut = "PUT"
	// TransferDefaultMaxSize default max size to transfer
	TransferDefaultMaxSize = 1099511627776 // 100MB
)

// Transfer transfer files via ssh
type Transfer struct {
	Inited         bool
	Method         string // GET,PUT
	LocalPath      string
	RemotePath     string
	Recursive      bool
	Hosts          []string
	Clients        map[string]*ssh.Client
	SftpClient     map[string]*sftp.Client
	Override       bool                    // override remote existed file?
	TransferResult map[string]FileTransfer // result of transfering
	Lock           sync.Mutex
}

// FileTransfer transfer file info
type FileTransfer struct {
	Source string
	Target string
	Size   int64
	Elapse time.Duration
}

// NewTransfer get file transfer instance
func NewTransfer(method, localPath, remotePath string, hosts []string) *Transfer {
	return &Transfer{
		Inited:         true,
		Method:         method,
		LocalPath:      localPath,
		RemotePath:     remotePath,
		Recursive:      false,
		Clients:        make(map[string]*ssh.Client),
		SftpClient:     make(map[string]*sftp.Client),
		Hosts:          hosts,
		Override:       false,
		TransferResult: make(map[string]FileTransfer),
		Lock:           sync.Mutex{},
	}
}

// Start start file transfer
func (t *Transfer) Start() (err error) {
	if err = t.initClient(); err != nil {
		return
	}
	// close connections
	defer func() {
		for _, sc := range t.SftpClient {
			sc.Close()
		}
		for _, c := range t.Clients {
			c.Close()
		}
	}()
	if t.Method == TransferGet {
		return t.batchGet()
	}
	if t.Method == TransferPut {
		return t.batchPut()
	}
	return nil
}

func (t *Transfer) batchGet() (err error) {
	fi, err := os.Stat(t.LocalPath)
	if err != nil {
		err = os.MkdirAll(t.LocalPath, 0755)
		if err != nil {
			return
		}
	} else {
		if !fi.IsDir() {
			log.Fatalln("Local path cannot be a file")
		}
	}
	wg := sync.WaitGroup{}
	for h, sc := range t.SftpClient {
		c := t.Clients[h]
		wg.Add(1)
		go func(sc *sftp.Client, c *ssh.Client) {
			defer wg.Done()
			err := t.get(sc, c, t.RemotePath, t.LocalPath)
			if err != nil {
				fmt.Println(c.Conn.RemoteAddr().String(), err)
			}
		}(sc, c)
	}
	wg.Wait()
	return
}

func (t *Transfer) batchPut() (err error) {
	fi, err := os.Stat(t.LocalPath)
	if err != nil {
		return
	}
	if fi.IsDir() {
		return errors.New("Local is dir,recursive transfer not supported now")
	}
	wg := sync.WaitGroup{}
	for h, sc := range t.SftpClient {
		c := t.Clients[h]
		wg.Add(1)
		go func(sc *sftp.Client, c *ssh.Client) {
			defer wg.Done()
			err := t.put(sc, c, t.LocalPath, t.RemotePath)
			if err != nil {
				fmt.Println(err)
			}
		}(sc, c)
	}
	wg.Wait()
	return
}

func (t *Transfer) get(sc *sftp.Client, c *ssh.Client, remotePath, localPath string) (err error) {
	fi, err := sc.Stat(remotePath)
	if err != nil {
		return
	}
	if fi.IsDir() {
		return errors.New("Remote dir get is not supported")
	}
	if fi.Size() > C.TransferMaxSize {
		return fmt.Errorf("Max transfer size is set to %d", C.TransferMaxSize)
	}
	basename := path.Base(fi.Name())
	srcFile, err := sc.Open(remotePath)
	if err != nil {
		return
	}
	defer srcFile.Close()
	addr := c.Conn.RemoteAddr().String()
	xaddr := strings.Split(addr, ":")
	exp := strings.Split(basename, ".")
	var ext, prefName string
	lenth := len(exp)
	if lenth > 1 {
		ext = exp[lenth-1]
		prefName = strings.Join(exp[0:lenth-1], ".")
	} else {
		prefName = basename
	}
	dstFile, err := os.OpenFile(path.Join(localPath, prefName+"-"+strings.Replace(xaddr[0], ".", "-", -1)+"."+ext), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return
	}
	defer dstFile.Close()
	ft := FileTransfer{
		Source: srcFile.Name(),
		Target: dstFile.Name(),
	}
	ts := time.Now()
	buf := make([]byte, 1024)
	var size int64
	for {
		n, _ := srcFile.Read(buf)
		if n < 1 {
			break
		}
		size = size + int64(n)
		dstFile.Write(buf[0:n])
	}
	ft.Size = size
	ft.Elapse = time.Now().Sub(ts)
	t.Lock.Lock()
	t.TransferResult[addr] = ft
	t.Lock.Unlock()
	return
}
func (t *Transfer) put(sc *sftp.Client, c *ssh.Client, localPath, remotePath string) (err error) {
	// remote path is dir
	if strings.HasSuffix(remotePath, "/") {
		basename := path.Base(localPath)
		remotePath = path.Join(remotePath, basename)
	}
	_, e := sc.Stat(remotePath)
	if e == nil {
		if !t.Override {
			fmt.Println("Remote file exists")
			return errors.New("Remote file exists")
		}
	}
	srcFile, err := os.OpenFile(localPath, os.O_RDONLY, 0755)
	if err != nil {
		return
	}
	defer srcFile.Close()
	dstFile, err := sc.OpenFile(remotePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return
	}
	defer dstFile.Close()
	ft := FileTransfer{
		Source: srcFile.Name(),
		Target: dstFile.Name(),
	}
	ts := time.Now()
	var size int64
	buf := make([]byte, 1024)
	for {
		n, _ := srcFile.Read(buf)
		if n < 1 {
			break
		}
		size = size + int64(n)
		dstFile.Write(buf[0:n])
	}
	ft.Size = size
	ft.Elapse = time.Now().Sub(ts)
	addr := c.Conn.RemoteAddr().String()
	t.Lock.Lock()
	t.TransferResult[addr] = ft
	t.Lock.Unlock()
	return
}

func (t *Transfer) initClient() error {
	auth, err := GetAuth()
	if err != nil {
		log.Fatalln(err)
	}
	clientConfig := &ssh.ClientConfig{
		User:            C.Auth.User,
		Auth:            auth,
		Timeout:         30 * time.Second,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	for _, h := range t.Hosts {
		if strings.Index(h, ":") < 0 {
			h = h + ":" + strconv.Itoa(C.Server.DefaultPort)
		}
		client, err := ssh.Dial("tcp", h, clientConfig)
		if err != nil {
			return err
		}
		t.Clients[h] = client
		t.SftpClient[h], err = sftp.NewClient(client, sftp.MaxPacket(33788))
		if err != nil {
			return err
		}
	}
	return nil
}

// PrettyPrint print transfer result
func (t *Transfer) PrettyPrint() {
	for h, ft := range t.TransferResult {
		fmt.Printf("%21s: %s => %s %dByte %.2f seconds\n", h, ft.Source, ft.Target, ft.Size, ft.Elapse.Seconds())
	}
}

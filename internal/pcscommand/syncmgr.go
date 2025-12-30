package pcscommand

import (
    "crypto/sha1"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "io/ioutil"
    "os"
    "os/signal"
    "path"
    "path/filepath"
    "strings"
    "sync"
    "syscall"
    "time"

    "github.com/qjfoidnh/BaiduPCS-Go/baidupcs"
    "github.com/qjfoidnh/BaiduPCS-Go/internal/pcsconfig"
    "github.com/qjfoidnh/BaiduPCS-Go/pcsutil"
)

const syncConfigFileName = "sync_config.json"

type WatchEntry struct {
    Local   string                       `json:"local"`
    Remote  string                       `json:"remote"`
    Interval int                         `json:"interval"`
    Key     string                       `json:"key,omitempty"`
    Method  string                       `json:"method,omitempty"`
    IgnoreFile string                     `json:"ignore_file,omitempty"`
    Files   map[string]syncFileState     `json:"files"`

    // runtime
    stopCh  chan struct{}                `json:"-"`
    // Running is runtime-only and should not be persisted to disk
    Running bool                         `json:"-"`
    patterns []ignorePattern            `json:"-"`
}

type SyncConfigFile struct {
    Watches map[string]*WatchEntry `json:"watches"`
}

var (
    mgrMu sync.Mutex
    mgr   = &syncManager{}
)

type syncManager struct{
    cfgPath string
    cfg     *SyncConfigFile
    mu      sync.Mutex
}

func (s *syncManager) configPath() string {
    if s.cfgPath != "" {
        return s.cfgPath
    }
    return filepath.Join(pcsconfig.GetConfigDir(), syncConfigFileName)
}

func (s *syncManager) load() error {
    s.mu.Lock()
    defer s.mu.Unlock()
    path := s.configPath()
    b, err := ioutil.ReadFile(path)
    if err != nil {
        if os.IsNotExist(err) {
            s.cfg = &SyncConfigFile{Watches: make(map[string]*WatchEntry)}
            return nil
        }
        return err
    }
    cfg := &SyncConfigFile{}
    if err := json.Unmarshal(b, cfg); err != nil {
        return err
    }
    if cfg.Watches == nil {
        cfg.Watches = make(map[string]*WatchEntry)
    }
    s.cfg = cfg
    return nil
}

func (s *syncManager) save() error {
    s.mu.Lock()
    defer s.mu.Unlock()
    if s.cfg == nil {
        s.cfg = &SyncConfigFile{Watches: make(map[string]*WatchEntry)}
    }
    b, err := json.MarshalIndent(s.cfg, "", "  ")
    if err != nil {
        return err
    }
    return ioutil.WriteFile(s.configPath(), b, 0644)
}

func (s *syncManager) AddWatch(local, remote string, interval int, key, method string) error {
    return s.AddWatchWithIgnore(local, remote, interval, key, method, "")
}

func (s *syncManager) AddWatchWithIgnore(local, remote string, interval int, key, method, ignoreFile string) error {
    mgrMu.Lock()
    defer mgrMu.Unlock()
    if err := s.load(); err != nil {
        return err
    }
    local = filepath.Clean(local)
    id := s.watchID(local)
    if _, ok := s.cfg.Watches[id]; ok {
        return fmt.Errorf("watch already exists for %s", local)
    }
    we := &WatchEntry{
        Local: local,
        Remote: remote,
        Interval: interval,
        Key: key,
        Method: method,
        IgnoreFile: ignoreFile,
        Files: make(map[string]syncFileState),
    }
    s.cfg.Watches[id] = we
    return s.save()
}

func (s *syncManager) DeleteWatch(local string) error {
    mgrMu.Lock()
    defer mgrMu.Unlock()
    if err := s.load(); err != nil {
        return err
    }
    id := s.watchID(filepath.Clean(local))
    if _, ok := s.cfg.Watches[id]; !ok {
        return fmt.Errorf("watch not found: %s", local)
    }
    // stop if running
    if s.cfg.Watches[id].Running && s.cfg.Watches[id].stopCh != nil {
        close(s.cfg.Watches[id].stopCh)
    }
    delete(s.cfg.Watches, id)
    return s.save()
}

func (s *syncManager) ListWatches() ([]*WatchEntry, error) {
    if err := s.load(); err != nil {
        return nil, err
    }
    res := make([]*WatchEntry, 0, len(s.cfg.Watches))
    for _, w := range s.cfg.Watches {
        res = append(res, w)
    }
    return res, nil
}

func (s *syncManager) StartWatch(local string) error {
    mgrMu.Lock()
    defer mgrMu.Unlock()
    if err := s.load(); err != nil {
        return err
    }
    id := s.watchID(filepath.Clean(local))
    w, ok := s.cfg.Watches[id]
    if !ok {
        return fmt.Errorf("watch not found: %s", local)
    }
    if w.Running {
        return fmt.Errorf("watch already running: %s", local)
    }
    w.stopCh = make(chan struct{})
    w.Running = true
    go s.runWatch(w)
    fmt.Printf("开始执行任务 本地目录 %s 同步 (interval=%d)\n", w.Local, w.Interval)
    return s.save()
}

func (s *syncManager) StopWatch(local string) error {
    mgrMu.Lock()
    defer mgrMu.Unlock()
    if err := s.load(); err != nil {
        return err
    }
    id := s.watchID(filepath.Clean(local))
    w, ok := s.cfg.Watches[id]
    if !ok {
        return fmt.Errorf("watch not found: %s", local)
    }
    if !w.Running {
        return fmt.Errorf("watch not running: %s", local)
    }
    if w.stopCh != nil {
        close(w.stopCh)
    }
    w.Running = false
    w.stopCh = nil
    return s.save()
}

func (s *syncManager) StartAll() error {
    if err := s.load(); err != nil {
        return err
    }
    for _, w := range s.cfg.Watches {
        if !w.Running {
            w.stopCh = make(chan struct{})
            w.Running = true
            go s.runWatch(w)
            fmt.Printf("开始执行任务 本地目录 %s 同步 (interval=%d)\n", w.Local, w.Interval)
        }
    }
    return s.save()
}

func (s *syncManager) StopAll() error {
    if err := s.load(); err != nil {
        return err
    }
    for _, w := range s.cfg.Watches {
        if w.Running && w.stopCh != nil {
            close(w.stopCh)
            w.Running = false
            w.stopCh = nil
        }
    }
    return s.save()
}

func (s *syncManager) runWatch(w *WatchEntry) {
    // run loop
    ticker := time.NewTicker(time.Duration(w.Interval) * time.Second)
    defer ticker.Stop()
    // immediate run
    s.scanAndUpload(w)
    for {
        select {
        case <-ticker.C:
            s.scanAndUpload(w)
        case <-w.stopCh:
            return
        }
    }
}

func (s *syncManager) scanAndUpload(w *WatchEntry) {
    walked, err := pcsutil.WalkDir(w.Local, "")
    if err != nil {
        fmt.Printf("walk %s error: %s\n", w.Local, err)
        return
    }
    for _, f := range walked {
        info, err := os.Stat(f)
        if err != nil || info.IsDir() {
            continue
        }
        fileUnix := filepath.ToSlash(filepath.Clean(f))
        baseUnix := filepath.ToSlash(w.Local)
        rel := strings.TrimPrefix(fileUnix, baseUnix)
        rel = strings.TrimPrefix(rel, "/")
        mod := info.ModTime().Unix()
        size := info.Size()
        prev, ok := w.Files[rel]
        if ok {
            if prev.ModTime == mod && prev.Size == size {
                continue
            }
            fmt.Printf("文件 %s 的修改时间/大小与配置中不一致: prev(mtime=%d,size=%d) new(mtime=%d,size=%d), 执行上传\n", rel, prev.ModTime, prev.Size, mod, size)
        } else {
            fmt.Printf("文件 %s 未在配置中, 执行首次上传\n", rel)
        }

        // check ignore rules
        if shouldIgnore(w, rel, info.IsDir()) {
            continue
        }
        // prepare upload
        uploadPath := f
        if w.Key != "" {
            tmp := f + ".encrypted"
            if err := encryptFileForSync(f, tmp, w.Key, w.Method); err != nil {
                fmt.Printf("encrypt error %s: %s\n", f, err)
                continue
            }
            uploadPath = tmp
        }
        relDir := filepath.Dir(rel)
        var savePath string
        if relDir == "." {
            savePath = w.Remote
        } else {
            savePath = path.Clean(w.Remote + baidupcs.PathSeparator + filepath.ToSlash(relDir))
        }
        fmt.Printf("[sync] %s -> %s\n", uploadPath, savePath)
        RunUpload([]string{uploadPath}, savePath, &UploadOptions{})
        // remove temporary encrypted file if any
        if uploadPath != f {
            os.Remove(uploadPath)
        }
        // update state
        if w.Files == nil {
            w.Files = make(map[string]syncFileState)
        }
        w.Files[rel] = syncFileState{ModTime: mod, Size: size}
        // persist
        if err := s.save(); err != nil {
            fmt.Printf("save sync config error: %s\n", err)
        }
    }
    // 完成本次扫描/上传
    next := time.Now().Add(time.Duration(w.Interval) * time.Second).Format(time.RFC3339)
    fmt.Printf("%s 同步完成, 下次同步时间为 %s\n", w.Local, next)
}

func (s *syncManager) watchID(local string) string {
    // use sha1 of absolute path as id
    h := sha1.Sum([]byte(local))
    return hex.EncodeToString(h[:])
}

// helper wrappers used by main
func AddSyncWatch(local, remote string, interval int, key, method string) error {
    return mgr.AddWatchWithIgnore(local, remote, interval, key, method, "")
}

// AddSyncWatchWithIgnore allows specifying an ignore file path (relative to local or absolute)
func AddSyncWatchWithIgnore(local, remote string, interval int, key, method, ignoreFile string) error {
    return mgr.AddWatchWithIgnore(local, remote, interval, key, method, ignoreFile)
}

func DeleteSyncWatch(local string) error {
    return mgr.DeleteWatch(local)
}

func ListSyncWatches() ([]*WatchEntry, error) {
    return mgr.ListWatches()
}

func StartSync(local string) error {
    if local == "" {
        if err := mgr.StartAll(); err != nil {
            return err
        }
    } else {
        if err := mgr.StartWatch(local); err != nil {
            return err
        }
    }

    // Block and wait for interrupt/terminate signal, then stop watches
    sigs := make(chan os.Signal, 1)
    signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
    <-sigs

    if local == "" {
        return mgr.StopAll()
    }
    return mgr.StopWatch(local)
}

func StopSync(local string) error {
    if local == "" {
        return mgr.StopAll()
    }
    return mgr.StopWatch(local)
}

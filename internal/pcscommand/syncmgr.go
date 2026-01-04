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

    "github.com/google/uuid"
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
    RandomFilename bool                  `json:"random_filename,omitempty"`
    Files   map[string]syncFileState     `json:"files"`
    FileNameMap map[string]string        `json:"file_name_map,omitempty"`

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
    return s.AddWatchWithIgnoreAndRandom(local, remote, interval, key, method, "", false)
}

func (s *syncManager) AddWatchWithIgnore(local, remote string, interval int, key, method, ignoreFile string) error {
    return s.AddWatchWithIgnoreAndRandom(local, remote, interval, key, method, ignoreFile, false)
}

func (s *syncManager) AddWatchWithIgnoreAndRandom(local, remote string, interval int, key, method, ignoreFile string, randomFilename bool) error {
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
        RandomFilename: randomFilename,
        Files: make(map[string]syncFileState),
        FileNameMap: make(map[string]string),
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

        // 计算文件的MD5
        currentMD5, err := md5sum(f)
        if err != nil {
            fmt.Printf("计算文件 %s 的MD5失败: %s, 跳过\n", rel, err)
            continue
        }

        prev, ok := w.Files[rel]
        if ok {
            if prev.MD5 == currentMD5 {
                continue
            }
            fmt.Printf("文件 %s 的MD5与配置中不一致: prev_md5=%s new_md5=%s, 执行上传\n", rel, prev.MD5, currentMD5)
        } else {
            fmt.Printf("文件 %s 未在配置中, 执行首次上传\n", rel)
        }

        // check ignore rules
        if shouldIgnore(w, rel, info.IsDir()) {
            continue
        }
        // prepare upload
        uploadPath := f
        uploadFileName := filepath.Base(rel)
        if w.Key != "" {
            tmp := f + ".encrypted"
            if err := encryptFileForSync(f, tmp, w.Key, w.Method); err != nil {
                fmt.Printf("encrypt error %s: %s\n", f, err)
                continue
            }
            uploadPath = tmp
            // 如果启用了随机文件名，生成UUID作为上传文件名
            if w.RandomFilename {
                newFileName := uuid.New().String()
                uploadFileName = newFileName
                if w.FileNameMap == nil {
                    w.FileNameMap = make(map[string]string)
                }
                w.FileNameMap[newFileName] = rel
                fmt.Printf("[sync] 生成随机文件名映射: %s -> %s\n", newFileName, rel)
            }
        }
        relDir := filepath.Dir(rel)
        var savePath string
        if relDir == "." {
            savePath = w.Remote
        } else {
            savePath = path.Clean(w.Remote + baidupcs.PathSeparator + filepath.ToSlash(relDir))
        }
        
        // 如果启用了随机文件名，创建临时文件并重命名
        if w.RandomFilename && w.Key != "" && uploadFileName != filepath.Base(rel) {
            tempUploadPath := filepath.Join(filepath.Dir(uploadPath), uploadFileName)
            if err := os.Rename(uploadPath, tempUploadPath); err != nil {
                fmt.Printf("rename error %s -> %s: %s\n", uploadPath, tempUploadPath, err)
                continue
            }
            uploadPath = tempUploadPath
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
        w.Files[rel] = syncFileState{ModTime: mod, Size: size, MD5: currentMD5}
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

// AddSyncWatchWithIgnoreAndRandom allows specifying an ignore file path and random filename option
func AddSyncWatchWithIgnoreAndRandom(local, remote string, interval int, key, method, ignoreFile string, randomFilename bool) error {
    return mgr.AddWatchWithIgnoreAndRandom(local, remote, interval, key, method, ignoreFile, randomFilename)
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

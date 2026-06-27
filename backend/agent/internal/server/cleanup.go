package server

import (
	"log"
	"os"
	"path/filepath"
	"time"
)

// StartWorkspaceGC 启动后台垃圾回收守护协程以清理过期会话区和裸仓库
// maxAge: 目录被保留的最长时间
// interval: 扫描间隔
func StartWorkspaceGC(maxAge time.Duration, interval time.Duration) {
	log.Printf("🧹 Workspace GC started: maxAge=%v, interval=%v", maxAge, interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// 启动时先执行一次清理
	performCleanup(maxAge)

	for range ticker.C {
		performCleanup(maxAge)
	}
}

func performCleanup(maxAge time.Duration) {
	now := time.Now()
	cleanDirs := []struct {
		baseDir  string
		preserve bool
	}{
		{baseDir: workspaceRoot(), preserve: true},
		{baseDir: filepath.Join(os.TempDir(), "repos"), preserve: false},
	}

	for _, target := range cleanDirs {
		baseDir := target.baseDir
		entries, err := os.ReadDir(baseDir)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("⚠️ [GC] Failed to read %s: %v", baseDir, err)
			}
			continue
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				// /tmp/repos 下的 bare repo 可能是目录;
				// 但如果里面混杂了文件，也应该跳过。
				continue
			}

			// 跳过一些被保留的全局目录
			if entry.Name() == ".global" {
				continue
			}

			dirPath := filepath.Join(baseDir, entry.Name())
			info, err := entry.Info()
			if err != nil {
				log.Printf("⚠️ [GC] Failed to get info for %s: %v", dirPath, err)
				continue
			}

			// 深度遍历目录，获取其内文件的最新修改时间，以防父目录 ModTime 未改变导致误删
			latestModTime := info.ModTime()
			_ = filepath.Walk(dirPath, func(path string, f os.FileInfo, err error) error {
				if err != nil {
					return nil // 忽略无权限或已删除的文件
				}
				if f.ModTime().After(latestModTime) {
					latestModTime = f.ModTime()
				}
				return nil
			})

			age := now.Sub(latestModTime)
			if age > maxAge {
				log.Printf("🗑️ [GC] Removing expired workspace/repo item: %s (age: %v)",
					dirPath, age.Round(time.Hour))

				if !target.preserve {
					if err := os.RemoveAll(dirPath); err != nil {
						log.Printf("💥 [GC] Failed to remove %s: %v", dirPath, err)
					}
					continue
				}
				items, err := os.ReadDir(dirPath)
				if err != nil {
					continue
				}
				for _, item := range items {
					if item.Name() != ".doops-audit-log" {
						itemPath := filepath.Join(dirPath, item.Name())
						if err := os.RemoveAll(itemPath); err != nil {
							log.Printf("💥 [GC] Failed to remove %s: %v", itemPath, err)
						}
					}
				}
				log.Printf("✅ [GC] Cleaned %s (preserved .doops-audit-log if any)", dirPath)
			}
		}
	}
}

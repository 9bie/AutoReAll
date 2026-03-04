package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	decompilerJar string
	outputDir     string
	uploadDir     string
	port          string
)

func main() {
	flag.StringVar(&decompilerJar, "decompiler", "", "Path to java-decompiler.jar (IntelliJ ConsoleDecompiler)")
	flag.StringVar(&outputDir, "output", "./output", "Directory for decompiled output")
	flag.StringVar(&uploadDir, "upload", "./uploads", "Directory for uploaded JAR files")
	flag.StringVar(&port, "port", "8080", "HTTP server listen port")
	flag.Parse()

	if decompilerJar == "" {
		log.Fatal("ERROR: -decompiler flag is required. Provide the path to java-decompiler.jar")
	}

	// Ensure directories exist
	os.MkdirAll(outputDir, 0755)
	os.MkdirAll(uploadDir, 0755)

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/upload", handleUpload)
	http.HandleFunc("/download/", handleDownload)

	addr := ":" + port
	log.Printf("🚀 ReJava Web server starting on http://localhost%s", addr)
	log.Printf("   Decompiler JAR: %s", decompilerJar)
	log.Printf("   Output Dir:     %s", outputDir)
	log.Printf("   Upload Dir:     %s", uploadDir)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// handleIndex serves the upload page
func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	tmpl, err := template.ParseFiles("templates/index.html")
	if err != nil {
		http.Error(w, "Failed to load template: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tmpl.Execute(w, nil)
}

// handleUpload receives JAR/WAR file, runs decompiler, returns result
func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit upload size to 2GB
	r.ParseMultipartForm(2 << 30)

	file, header, err := r.FormFile("jarfile")
	if err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Failed to read uploaded file: " + err.Error(),
		})
		return
	}
	defer file.Close()

	// Validate file extension
	lowerName := strings.ToLower(header.Filename)
	if !strings.HasSuffix(lowerName, ".jar") && !strings.HasSuffix(lowerName, ".war") {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Only .jar / .war files are accepted",
		})
		return
	}

	// Create a unique task directory
	taskID := fmt.Sprintf("%d", time.Now().UnixNano())
	taskUploadDir := filepath.Join(uploadDir, taskID)
	taskOutputDir := filepath.Join(outputDir, taskID)
	os.MkdirAll(taskUploadDir, 0755)
	os.MkdirAll(taskOutputDir, 0755)

	// Save uploaded file
	jarPath := filepath.Join(taskUploadDir, header.Filename)
	dst, err := os.Create(jarPath)
	if err != nil {
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   "Failed to save file: " + err.Error(),
		})
		return
	}
	io.Copy(dst, file)
	dst.Close()

	log.Printf("📦 Received: %s (%d bytes), Task ID: %s", header.Filename, header.Size, taskID)

	// ══════════════════════════════════════════════════════════
	// Step 1: Decompile the uploaded JAR/WAR
	// ══════════════════════════════════════════════════════════
	var allLog strings.Builder

	allLog.WriteString("═══ Step 1: Decompiling uploaded file ═══\n")
	cmd := exec.Command("java", "-cp", decompilerJar,
		"org.jetbrains.java.decompiler.main.decompiler.ConsoleDecompiler",
		"-dgs=true", jarPath, taskOutputDir)

	output, err := cmd.CombinedOutput()
	allLog.Write(output)

	if err != nil {
		log.Printf("❌ Decompile failed for task %s: %v", taskID, err)
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success": false,
			"error":   "Decompile failed: " + err.Error(),
			"log":     allLog.String(),
			"taskId":  taskID,
		})
		return
	}

	log.Printf("✅ Step 1 decompile success for task %s", taskID)

	// ══════════════════════════════════════════════════════════
	// Step 2: Extract decompiled output and process WEB-INF/lib
	// ══════════════════════════════════════════════════════════
	allLog.WriteString("\n═══ Step 2: Post-processing WEB-INF/lib ═══\n")
	postLog := postProcessWAR(jarPath, taskOutputDir, taskUploadDir)
	allLog.WriteString(postLog)

	log.Printf("✅ All steps complete for task %s", taskID)

	// Create final zip of output
	allLog.WriteString("\n═══ Step 3: Packaging results ═══\n")
	zipPath := filepath.Join(outputDir, taskID+".zip")
	if err := createZip(taskOutputDir, zipPath); err != nil {
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"log":     allLog.String(),
			"taskId":  taskID,
			"error":   "Decompile succeeded but failed to create zip: " + err.Error(),
		})
		return
	}

	// Get zip size for log
	if zipInfo, err := os.Stat(zipPath); err == nil {
		sizeMB := float64(zipInfo.Size()) / 1024 / 1024
		allLog.WriteString(fmt.Sprintf("📦 ZIP 已创建: %.2f MB\n", sizeMB))
	}

	// ══════════════════════════════════════════════════════════
	// Step 4: Cleanup — remove decompile cache directories
	// ══════════════════════════════════════════════════════════
	allLog.WriteString("\n═══ Step 4: Cleaning up cache ═══\n")

	// Remove the task output directory (extracted files, no longer needed after zip)
	if err := os.RemoveAll(taskOutputDir); err != nil {
		allLog.WriteString(fmt.Sprintf("⚠ 清理输出目录失败: %v\n", err))
	} else {
		allLog.WriteString(fmt.Sprintf("🗑 已删除输出缓存: %s\n", taskOutputDir))
	}

	// Remove the upload directory (uploaded file + temp extracts)
	if err := os.RemoveAll(taskUploadDir); err != nil {
		allLog.WriteString(fmt.Sprintf("⚠ 清理上传目录失败: %v\n", err))
	} else {
		allLog.WriteString(fmt.Sprintf("🗑 已删除上传缓存: %s\n", taskUploadDir))
	}

	allLog.WriteString("✅ 清理完成\n")

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success":     true,
		"log":         allLog.String(),
		"taskId":      taskID,
		"downloadUrl": "/download/" + taskID,
	})
}

// ═══════════════════════════════════════════════════════════════
// postProcessWAR: after initial decompile, recursively scan
// ALL JARs for matching package names, decompile & merge them
// ═══════════════════════════════════════════════════════════════
func postProcessWAR(originalJarPath, taskOutputDir, taskUploadDir string) string {
	var logBuf strings.Builder

	// Find the decompiled output jar/war (ConsoleDecompiler puts a jar with same name in output dir)
	decompiledFile := ""
	filepath.Walk(taskOutputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && (strings.HasSuffix(path, ".jar") || strings.HasSuffix(path, ".war")) {
			decompiledFile = path
			return filepath.SkipAll
		}
		return nil
	})

	if decompiledFile == "" {
		logBuf.WriteString("⚠ 输出目录中未找到反编译产物 JAR/WAR，跳过后续处理\n")
		return logBuf.String()
	}

	logBuf.WriteString(fmt.Sprintf("📄 反编译产物: %s\n", filepath.Base(decompiledFile)))

	// Extract the decompiled output
	extractDir := filepath.Join(taskOutputDir, "_extracted")
	if err := extractZip(decompiledFile, extractDir); err != nil {
		logBuf.WriteString(fmt.Sprintf("⚠ 提取反编译产物失败: %v\n", err))
		return logBuf.String()
	}

	// ── Guess package name ──
	// Recursively find all directories named 'classes' in the extracted output
	var classesDirs []string
	filepath.Walk(extractDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && info.Name() == "classes" {
			classesDirs = append(classesDirs, path)
		}
		return nil
	})

	// Determine the package analysis source and merge target
	var analysisDir string
	var mergeTargetDir string

	if len(classesDirs) > 0 {
		analysisDir = classesDirs[0]
		mergeTargetDir = classesDirs[0]
		relClassesPath, _ := filepath.Rel(extractDir, classesDirs[0])
		logBuf.WriteString(fmt.Sprintf("ℹ 发现 classes 目录: %s\n", relClassesPath))
		if len(classesDirs) > 1 {
			for _, d := range classesDirs[1:] {
				rel, _ := filepath.Rel(extractDir, d)
				logBuf.WriteString(fmt.Sprintf("  （另有 classes 目录: %s）\n", rel))
			}
		}
	} else {
		logBuf.WriteString("ℹ 未找到 classes 目录，将从反编译根目录分析包名\n")
		analysisDir = extractDir
		mergeTargetDir = extractDir
	}

	analysis := guessPackageFromClasses(analysisDir)
	logBuf.WriteString(analysis.debugLog)

	if len(analysis.packages) == 0 {
		logBuf.WriteString("⚠ 无法从反编译结果中确定包名\n")
		os.Remove(decompiledFile)
		return logBuf.String()
	}
	packages := analysis.packages
	logBuf.WriteString(fmt.Sprintf("\n🔍 将使用以下包名在原始文件中查找匹配的 JAR: %s\n", strings.Join(packages, ", ")))

	// ── Extract original JAR/WAR to find all embedded JARs ──
	originalExtractDir := filepath.Join(taskUploadDir, "_original_extracted")
	if err := extractZip(originalJarPath, originalExtractDir); err != nil {
		logBuf.WriteString(fmt.Sprintf("⚠ 提取原始文件失败: %v\n", err))
		os.Remove(decompiledFile)
		return logBuf.String()
	}

	// ── Recursively find ALL .jar files in the entire extracted directory ──
	var allJars []string
	filepath.Walk(originalExtractDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(strings.ToLower(info.Name()), ".jar") {
			allJars = append(allJars, path)
		}
		return nil
	})

	logBuf.WriteString(fmt.Sprintf("📚 在原始文件中共找到 %d 个 JAR 文件\n", len(allJars)))

	if len(allJars) == 0 {
		logBuf.WriteString("ℹ 没有找到内嵌 JAR，跳过\n")
		os.Remove(decompiledFile)
		// Move extracted to root
		moveExtractedToRoot(extractDir, taskOutputDir)
		return logBuf.String()
	}

	// ── Scan all JARs for matching packages ──
	matchCount := 0
	skipCount := 0
	for _, libJar := range allJars {
		// Display relative path for readability
		relJarPath, _ := filepath.Rel(originalExtractDir, libJar)
		jarName := filepath.Base(libJar)

		matched, matchedPkg := jarContainsPackage(libJar, packages)
		if !matched {
			skipCount++
			logBuf.WriteString(fmt.Sprintf("  ⏭ %s — 不匹配，跳过\n", relJarPath))
			continue
		}

		matchCount++
		logBuf.WriteString(fmt.Sprintf("\n  🎯 [%d] %s 匹配包名 '%s'\n", matchCount, relJarPath, matchedPkg))

		// Decompile this JAR
		libDecompileDir := filepath.Join(taskUploadDir, "_lib_decompile", jarName)
		os.MkdirAll(libDecompileDir, 0755)

		cmd := exec.Command("java", "-cp", decompilerJar,
			"org.jetbrains.java.decompiler.main.decompiler.ConsoleDecompiler",
			"-dgs=true", libJar, libDecompileDir)

		out, err := cmd.CombinedOutput()
		if err != nil {
			logBuf.WriteString(fmt.Sprintf("     ❌ 反编译失败: %v\n", err))
			logBuf.Write(out)
			continue
		}

		logBuf.WriteString("     ✅ 反编译成功\n")

		// Find the decompiled jar in the output
		libDecompiledFile := ""
		filepath.Walk(libDecompileDir, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && strings.HasSuffix(p, ".jar") {
				libDecompiledFile = p
				return filepath.SkipAll
			}
			return nil
		})

		if libDecompiledFile == "" {
			logBuf.WriteString("     ⚠ 未找到反编译输出\n")
			continue
		}

		// Extract decompiled lib jar and merge into target dir
		libExtractDir := filepath.Join(taskUploadDir, "_lib_extract", jarName)
		if err := extractZip(libDecompiledFile, libExtractDir); err != nil {
			logBuf.WriteString(fmt.Sprintf("     ⚠ 提取失败: %v\n", err))
			continue
		}

		// Merge: copy all files from lib extract into the merge target, overwrite on conflict
		merged := 0
		overwritten := 0
		filepath.Walk(libExtractDir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			relPath, _ := filepath.Rel(libExtractDir, p)
			destPath := filepath.Join(mergeTargetDir, relPath)
			os.MkdirAll(filepath.Dir(destPath), 0755)

			// Check if file already exists (for overwrite count)
			if _, err := os.Stat(destPath); err == nil {
				overwritten++
			}

			src, err := os.Open(p)
			if err != nil {
				return nil
			}
			defer src.Close()

			dst, err := os.Create(destPath) // overwrite on conflict
			if err != nil {
				return nil
			}
			defer dst.Close()

			io.Copy(dst, src)
			merged++
			return nil
		})
		logBuf.WriteString(fmt.Sprintf("     📝 合并 %d 个文件（其中 %d 个覆盖已有文件）\n", merged, overwritten))
	}

	logBuf.WriteString(fmt.Sprintf("\n═══ 汇总: %d 匹配 / %d 跳过 / %d 总计 ═══\n", matchCount, skipCount, len(allJars)))

	// Remove the original decompiled jar/war, keep extracted as final output
	os.Remove(decompiledFile)

	// Move extracted contents to taskOutputDir root
	moveExtractedToRoot(extractDir, taskOutputDir)

	return logBuf.String()
}

// moveExtractedToRoot moves contents of extractDir into taskOutputDir
func moveExtractedToRoot(extractDir, taskOutputDir string) {
	entries, _ := os.ReadDir(extractDir)
	for _, entry := range entries {
		src := filepath.Join(extractDir, entry.Name())
		dst := filepath.Join(taskOutputDir, entry.Name())
		os.RemoveAll(dst)
		os.Rename(src, dst)
	}
	os.RemoveAll(extractDir)
}

// packageAnalysis holds the analysis result of WEB-INF/classes
type packageAnalysis struct {
	packages []string // selected root packages for matching
	debugLog string   // detailed debug info
}

// Spring Boot / common framework packages to ignore during matching
var ignoredPackages = map[string]bool{
	"org/springframework": true,
	"org/apache":          true,
	"org/hibernate":       true,
	"org/mybatis":         true,
	"org/thymeleaf":       true,
	"org/slf4j":           true,
	"org/aspectj":         true,
	"org/jboss":           true,
	"org/eclipse":         true,
	"org/yaml":            true,
	"org/json":            true,
	"org/xml":             true,
	"org/w3c":             true,
	"org/ietf":            true,
	"org/objectweb":       true,
	"org/aopalliance":     true,
	"org/attoparser":      true,
	"org/unbescape":       true,
	"org/reactivestreams": true,
	"com/google":          true,
	"com/fasterxml":       true,
	"com/zaxxer":          true,
	"com/sun":             true,
	"com/mysql":           true,
	"com/microsoft":       true,
	"com/alibaba":         true,
	"com/baomidou":        true,
	"com/github":          true,
	"jakarta/servlet":     true,
	"jakarta/annotation":  true,
	"javax/servlet":       true,
	"javax/annotation":    true,
	"javax/persistence":   true,
	"javax/validation":    true,
	"io/netty":            true,
	"io/micrometer":       true,
	"io/swagger":          true,
	"net/bytebuddy":       true,
	"net/minidev":         true,
	"ch/qos":              true,
	"redis/clients":       true,
	"javassist":           true,
	"META-INF":            true,
}

// isIgnoredPackage checks if a package path starts with any ignored prefix
func isIgnoredPackage(pkgPath string) bool {
	parts := strings.Split(pkgPath, "/")
	if len(parts) >= 1 {
		if ignoredPackages[parts[0]] {
			return true
		}
	}
	if len(parts) >= 2 {
		if ignoredPackages[strings.Join(parts[:2], "/")] {
			return true
		}
	}
	return false
}

// guessPackageFromClasses scans a classes directory to detect the root package name(s)
// Spring Boot and common framework packages are automatically ignored
func guessPackageFromClasses(classesDir string) packageAnalysis {
	var dbg strings.Builder

	packageCount := make(map[string]int)
	packageCount3 := make(map[string]int)
	ignoredCount := make(map[string]int)
	totalFiles := 0

	filepath.Walk(classesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".java" && ext != ".class" {
			return nil
		}

		totalFiles++
		relPath, _ := filepath.Rel(classesDir, path)
		dir := filepath.Dir(relPath)
		if dir == "." {
			return nil
		}

		parts := strings.Split(filepath.ToSlash(dir), "/")
		if len(parts) >= 2 {
			base := strings.Join(parts[:2], "/")

			if isIgnoredPackage(base) {
				ignoredCount[base]++
				return nil
			}

			packageCount[base]++

			if len(parts) >= 3 {
				deeper := strings.Join(parts[:3], "/")
				packageCount3[deeper]++
			}
		}
		return nil
	})

	dbg.WriteString(fmt.Sprintf("\n  📊 classes 目录分析结果 (共 %d 个源文件):\n", totalFiles))

	if len(ignoredCount) > 0 {
		dbg.WriteString("  ┌─ 已忽略的框架包名:\n")
		for pkg, cnt := range ignoredCount {
			dotPkg := strings.ReplaceAll(pkg, "/", ".")
			dbg.WriteString(fmt.Sprintf("  │  %-40s  %d 个文件 (忽略)\n", dotPkg, cnt))
		}
	}

	if len(packageCount) == 0 {
		dbg.WriteString("  ⚠ 未找到任何包结构\n")
		return packageAnalysis{debugLog: dbg.String()}
	}

	// Sort and display ALL 2-level packages
	type pkgInfo struct {
		path  string
		count int
	}
	var pkgs2 []pkgInfo
	for p, c := range packageCount {
		pkgs2 = append(pkgs2, pkgInfo{p, c})
	}
	sort.Slice(pkgs2, func(i, j int) bool {
		return pkgs2[i].count > pkgs2[j].count
	})

	dbg.WriteString("  ┌─ 二级包名统计 (用于匹配):\n")
	for _, p := range pkgs2 {
		dotPkg := strings.ReplaceAll(p.path, "/", ".")
		dbg.WriteString(fmt.Sprintf("  │  %-40s  %d 个文件\n", dotPkg, p.count))
	}

	// Sort and display ALL 3-level packages
	if len(packageCount3) > 0 {
		var pkgs3 []pkgInfo
		for p, c := range packageCount3 {
			pkgs3 = append(pkgs3, pkgInfo{p, c})
		}
		sort.Slice(pkgs3, func(i, j int) bool {
			return pkgs3[i].count > pkgs3[j].count
		})

		dbg.WriteString("  ├─ 三级包名统计 (更精确):\n")
		for _, p := range pkgs3 {
			dotPkg := strings.ReplaceAll(p.path, "/", ".")
			dbg.WriteString(fmt.Sprintf("  │  %-40s  %d 个文件\n", dotPkg, p.count))
		}
	}

	// Select top packages for matching
	seen := make(map[string]bool)
	var result []string
	for _, p := range pkgs2 {
		parts := strings.Split(p.path, "/")
		var key string
		if len(parts) >= 2 {
			key = strings.Join(parts[:2], "/")
		} else {
			key = parts[0]
		}
		if !seen[key] {
			seen[key] = true
			result = append(result, strings.ReplaceAll(key, "/", "."))
			if len(result) >= 3 {
				break
			}
		}
	}

	dbg.WriteString(fmt.Sprintf("  └─ 选定匹配包名: %s\n", strings.Join(result, ", ")))

	return packageAnalysis{packages: result, debugLog: dbg.String()}
}

// jarContainsPackage checks if a JAR file contains classes under any of the given packages
func jarContainsPackage(jarPath string, packages []string) (bool, string) {
	r, err := zip.OpenReader(jarPath)
	if err != nil {
		return false, ""
	}
	defer r.Close()

	// Convert dot-separated packages to path prefixes
	var prefixes []string
	for _, pkg := range packages {
		prefixes = append(prefixes, strings.ReplaceAll(pkg, ".", "/")+"/")
	}

	for _, f := range r.File {
		name := f.Name
		if !strings.HasSuffix(name, ".class") {
			continue
		}
		for i, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				return true, packages[i]
			}
		}
	}
	return false, ""
}

// extractZip extracts a zip/jar/war file to a destination directory
func extractZip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()

	for _, f := range r.File {
		fpath := filepath.Join(dest, f.Name)

		// Security: prevent zip slip
		if !strings.HasPrefix(filepath.Clean(fpath), filepath.Clean(dest)+string(os.PathSeparator)) {
			continue
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, 0755)
			continue
		}

		os.MkdirAll(filepath.Dir(fpath), 0755)

		outFile, err := os.Create(fpath)
		if err != nil {
			return fmt.Errorf("create %s: %w", fpath, err)
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return fmt.Errorf("open entry %s: %w", f.Name, err)
		}

		io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
	}
	return nil
}

// handleDownload serves the zipped decompiled output
func handleDownload(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimPrefix(r.URL.Path, "/download/")
	if taskID == "" {
		http.Error(w, "Missing task ID", http.StatusBadRequest)
		return
	}

	zipPath := filepath.Join(outputDir, taskID+".zip")
	if _, err := os.Stat(zipPath); os.IsNotExist(err) {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"decompiled_%s.zip\"", taskID))
	http.ServeFile(w, r, zipPath)
}

// createZip creates a zip file from a directory
func createZip(srcDir, destZip string) error {
	zipFile, err := os.Create(destZip)
	if err != nil {
		return err
	}
	defer zipFile.Close()

	w := zip.NewWriter(zipFile)
	defer w.Close()

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		writer, err := w.Create(relPath)
		if err != nil {
			return err
		}

		reader, err := os.Open(path)
		if err != nil {
			return err
		}
		defer reader.Close()

		_, err = io.Copy(writer, reader)
		return err
	})
}

// sendJSON writes a JSON response
func sendJSON(w http.ResponseWriter, status int, data map[string]interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	var parts []string
	for k, v := range data {
		switch val := v.(type) {
		case string:
			escaped := strings.ReplaceAll(val, "\\", "\\\\")
			escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
			escaped = strings.ReplaceAll(escaped, "\n", "\\n")
			escaped = strings.ReplaceAll(escaped, "\r", "\\r")
			escaped = strings.ReplaceAll(escaped, "\t", "\\t")
			parts = append(parts, fmt.Sprintf(`"%s":"%s"`, k, escaped))
		case bool:
			parts = append(parts, fmt.Sprintf(`"%s":%t`, k, val))
		case int:
			parts = append(parts, fmt.Sprintf(`"%s":%d`, k, val))
		default:
			parts = append(parts, fmt.Sprintf(`"%s":"%v"`, k, val))
		}
	}
	fmt.Fprintf(w, "{%s}", strings.Join(parts, ","))
}

# ReJava Web

将 IntelliJ ConsoleDecompiler 的 JAR 反编译功能封装为 Web 服务，支持上传 JAR/WAR 文件一键反编译，自动识别同包名依赖 JAR 并合并反编译。

## 功能

- 📦 **上传反编译** — 拖拽上传 JAR/WAR 文件，自动调用 ConsoleDecompiler 反编译
- 🔍 **智能扫描** — 自动分析反编译结果中的包名结构，递归扫描原始文件中所有内嵌 JAR
- 🎯 **包名匹配** — 对包名匹配的 JAR 自动进行二次反编译，合并结果（冲突覆盖）
- 📥 **打包下载** — 反编译结果打包为 ZIP 下载，完成后自动清理缓存
- 🎨 **现代界面** — 深色主题 + 拖拽上传 + 实时进度 + 详细日志

## 前置条件

- Go 1.18+
- Java（`java` 命令在 PATH 中）
- IntelliJ IDEA（需要 `java-decompiler.jar`）

## 构建

```bash
go build -o rejava-web .
```

## 使用

```bash
# 必须用 -decompiler 指定 java-decompiler.jar 路径
./rejava-web -decompiler "/path/to/java-decompiler.jar"

# 示例 (macOS IntelliJ)
./rejava-web -decompiler "/Applications/IntelliJ IDEA.app/Contents/plugins/java-decompiler/lib/java-decompiler.jar"
```

浏览器打开 `http://localhost:8080`

### 可选参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-decompiler` | *(必填)* | java-decompiler.jar 路径 |
| `-port` | `8080` | 监听端口 |
| `-output` | `./output` | 反编译输出目录 |
| `-upload` | `./uploads` | 上传暂存目录 |

## 工作流程

```
上传 JAR/WAR
    ↓
Step 1: 反编译主文件
    ↓
Step 2: 分析包名 → 递归扫描内嵌 JAR → 匹配反编译 → 合并覆盖
    ↓
Step 3: 打包为 ZIP
    ↓
Step 4: 清理缓存 → 返回下载链接
```

## 项目结构

```
AutoReAll/
├── main.go              # Go 后端（HTTP 服务 + 反编译逻辑）
├── templates/
│   └── index.html       # 前端页面
├── go.mod
├── rejava.bat           # 原始批处理脚本（参考）
└── README.md
```

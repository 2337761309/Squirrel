# 子域名存活性检测工具

这是一个使用Go语言编写的工具，用于批量检测子域名是否存活。

## 功能

- 批量检测多个子域名的存活状态
- 详细显示HTTP状态码及对应状态（如"存活"、"禁止访问"、"未找到"等）
- 支持从文件中读取域名列表
- 支持直接从命令行输入域名列表
- 自动识别域名应使用HTTP还是HTTPS协议（优先尝试HTTPS）
- 自动提取并识别页面重要信息（登录页面、管理后台、API等）
- 自定义并发数量，高效检测大量域名
- 实时输出检测结果，无需等待所有域名检测完成
- 可设置请求超时时间
- 支持记录响应时间
- 跟随/不跟随HTTP重定向
- 可输出结果到CSV文件
- 可输出结果到Excel文件（支持只导出存活域名）
- 支持详细输出模式
- **自动截图存活页面并保存**（新功能）
- **自动截图所有网页并保存（包括错误页面）**（新功能）
- **生成包含截图的HTML报告**（新功能）

## 安装

确保你已经安装了Go语言环境（要求Go 1.13或更高版本）。

```bash
# 克隆仓库
git clone https://github.com/2337761309/Squirrel
cd Squirrel

# 构建
# Windows
go build -o squirrel.exe

# Linux/macOS
chmod +x build.sh
./build.sh

# or
go build -o squirrel
```

或者直接使用go install：

```bash
go install github.com/2337761309/Squirrel@latest
```

## 使用方法

### 命令行参数

```
用法: squirrel [选项] <域名列表文件或逗号分隔的域名列表>

选项:
  -concurrency int
        并发数量 (默认 10)
  -extract
        提取页面重要信息（登录页面等）
  -follow
        跟随重定向
  -output string
        输出结果到CSV文件
  -excel string
        输出结果到Excel文件
  -only-alive
        只导出存活的域名（与-output或-excel一起使用）
  -screenshot
        对所有网页进行截图（包括错误页面）
  -screenshot-alive
        只截图存活的网页
  -screenshot-dir string
        截图保存目录 (默认 "screenshots")
  -simple-html string
        输出结果到简化版HTML文件
  -html string
        输出结果到HTML文件
  -time
        显示响应时间
  -timeout int
        请求超时时间(秒) (默认 10)
  -verbose
        显示详细输出
```

### 从文件读取域名列表

创建一个文本文件，每行一个域名：

```
example.com
sub1.example.com
sub2.example.com
# 这是注释行，会被忽略
```

然后运行：

```bash
./squirrel domains.txt
```

### 直接指定域名列表

```bash
./squirrel example.com,sub1.example.com,sub2.example.com
```

### 自定义并发和超时

```bash
./squirrel -concurrency 20 -timeout 5 domains.txt
```

### 显示响应时间并输出详细信息

```bash
./squirrel -time -verbose domains.txt
```

### 保存结果到CSV文件

```bash
./squirrel -output results.csv domains.txt
```

### 保存结果到Excel文件

```bash
./squirrel -excel results.xlsx domains.txt
```

### 只导出存活的域名到Excel

```bash
./squirrel -excel alive_domains.xlsx -only-alive domains.txt
```

### 截图所有网页（包括错误页面）并保存到Excel

```bash
./squirrel -excel screenshots.xlsx -screenshot domains.txt
```

### 只截图存活网页并保存到Excel

```bash
./squirrel -excel screenshots.xlsx -screenshot-alive domains.txt
```

### 截图并只导出存活域名

```bash
./squirrel -excel screenshots.xlsx -screenshot-alive -only-alive domains.txt
```

### 生成包含截图的HTML报告

```bash
./squirrel -screenshot -simple-html index.html domains.txt
```

### 生成只包含存活网站截图的HTML报告

```bash
./squirrel -screenshot-alive -simple-html alive-sites.html domains.txt
```

### 提取页面重要信息

```bash
./squirrel -extract domains.txt
```

与详细输出组合使用，获取更多信息：

```bash
./squirrel -extract -verbose domains.txt
```

### 完整的命令示例

以下示例展示了使用所有主要功能的命令：

```bash
./squirrel -concurrency 30 -timeout 15 -extract -follow -time -verbose -excel results.xlsx -screenshot-alive -only-alive domains.txt
```

这个命令将：
- 使用30个并发线程
- 设置15秒请求超时
- 提取页面重要信息
- 跟随HTTP重定向
- 显示响应时间
- 使用详细输出模式
- 将结果保存到Excel文件
- 截图存活的网页
- 只导出存活的域名
- 检查domains.txt中的所有域名

### 不同截图模式的区别

工具提供了两种截图模式：

1. **截图所有网页** (`-screenshot`): 不管网站状态如何，都会对每个域名进行截图，包括返回404、403等错误状态码的页面，甚至是连接失败的页面也会生成错误截图。适用于希望全面了解所有域名的情况。

```bash
./squirrel -excel all-screenshots.xlsx -screenshot domains.txt
```

2. **只截图存活网页** (`-screenshot-alive`): 只对存活的网站（状态码<400，如200、301、302等）进行截图。适用于只关注可访问的网站。

```bash
./squirrel -excel alive-screenshots.xlsx -screenshot-alive domains.txt
```

## 输出示例

### 基本输出

```
总共需要检测 3 个域名，并发数: 10，超时: 10秒

检测结果 (实时输出):
----------------------------------------
域名                                     状态       状态码     响应时间(ms)   截图
----------------------------------------
https://example.com                      存活       200        187.25        [已截图]
http://sub1.example.com                 未找到      404        203.50       
https://sub2.example.com                禁止访问    403        231.12       
----------------------------------------
总计: 3 个域名, 1 个存活, 2 个无法访问
成功截图存活网站: 1 个
检测耗时: 1.24 秒
```

### 包含页面信息提取的输出

```
总共需要检测 3 个域名，并发数: 10，超时: 10秒

检测结果 (实时输出):
----------------------------------------
域名                                     状态       状态码     页面类型        页面标题                        截图
----------------------------------------
https://example.com                      存活       200        -               Example Domain                 [已截图]
http://login.example.com                存活       200        登录页面        Login - Example                [已截图]
https://admin.example.com               禁止访问    403        -               Access Denied                
----------------------------------------
总计: 3 个域名, 2 个存活, 1 个无法访问
页面类型统计:
  登录页面: 1 个
成功截图存活网站: 2 个
检测耗时: 1.54 秒
```

## Excel输出格式

使用`-excel`参数输出的Excel文件包含以下列：
- 域名
- 状态（存活、重定向、禁止访问等）
- 状态码（200、404、403等）
- 响应时间（毫秒）
- 页面类型（如果启用了-extract选项）
- 页面标题
- 消息（通常是状态码的文本描述）
- 截图（如果启用了-screenshot或-screenshot-alive选项，会显示"查看截图"链接）

当使用截图选项时，Excel文件会包含两个工作表：
1. **子域名检测结果** - 包含所有检测数据和到截图的链接
2. **页面截图** - 包含每个被截图网页的截图

使用`-only-alive`选项时，Excel文件中将只包含状态为"存活"的域名。

## HTML输出格式

使用`-simple-html`或`-html`选项时，程序将生成一个美观的HTML报告，其中包含：
- 检测统计信息摘要
- 按卡片形式组织的每个域名结果
- 域名的所有信息（状态、响应时间、页面类型等）
- 当启用截图选项时，HTML中会包含网站截图

HTML报告可以在任何浏览器中查看，是分享结果的理想方式。

## 截图功能

截图功能使用headless Chrome浏览器来捕获网页的可视化内容。要使用此功能：

1. 确保你的系统上安装了Chrome或Chromium浏览器
2. 使用`-screenshot`（所有网页）或`-screenshot-alive`（仅存活网页）选项来启用截图功能
3. 必须同时使用`-excel`、`-html`或`-simple-html`选项指定输出文件

截图功能注意事项：
- `-screenshot`参数会对所有网页进行截图，无论状态如何
- `-screenshot-alive`参数只对状态为"存活"的域名进行截图（状态码<400）
- 截图过程可能会使检测速度稍慢（取决于网页加载速度）
- 截图会使Excel文件体积增大
- 截图会在Excel工作表中自动缩放为原尺寸的30%以便查看
- 默认情况下，截图保存在当前目录下的"screenshots"文件夹中
- 可以使用`-screenshot-dir`选项自定义截图保存目录

## 状态显示

工具会根据HTTP状态码显示不同的状态文本：

| 状态码 | 显示状态   | 被归类为 |
|--------|------------|---------|
| 200    | 存活       | 存活    |
| 301/302| 重定向     | 存活    |
| 403    | 禁止访问   | 无法访问 |
| 404    | 未找到     | 无法访问 |
| 500    | 服务器错误 | 无法访问 |
| 502    | 网关错误   | 无法访问 |
| 503    | 服务不可用 | 无法访问 |
| 其他<400| 存活      | 存活    |
| 其他>=400| 无法访问 | 无法访问 |

## 可识别的页面类型

该工具可以识别以下类型的页面：

1. **登录页面** - 包含登录表单、用户名/密码输入框的页面
2. **管理后台** - 管理系统、控制面板、后台管理页面
3. **API接口** - REST API、GraphQL接口或API文档
4. **上传页面** - 包含文件上传功能的页面

## 注意事项

- 默认请求超时时间为10秒
- 默认并发数为10
- 状态码小于400的网站被认为是存活的
- 如果域名不包含协议前缀，将优先尝试HTTPS连接，连接失败再尝试HTTP
- 域名列表文件中以#开头的行会被视为注释并忽略
- 对于大量域名（尤其是超过500个），程序会实时输出检测结果，不必等待所有检测完成
- 页面类型识别依赖于页面内容分析，可能存在误判
- 截图功能需要Chrome/Chromium浏览器支持 

@echo off
REM =====================================================================
REM  WinPE 启动后自动执行脚本示例（startnet.cmd）
REM  用途：在自建 PE（WIM）中无人值守地挂载网络安装源并启动 Windows setup。
REM  用法：用 dism 把本文件注入你的 boot.wim：
REM          dism /Mount-Wim /WimFile:boot.wim /Index:1 /MountDir:mount
REM          copy startnet.cmd mount\Windows\System32\startnet.cmd
REM          dism /Unmount-Wim /MountDir:mount /Commit
REM  然后经本服务的 wim 模板（public-wim）启动该 WIM。
REM =====================================================================

REM 1) 初始化 PE 网络（加载网卡驱动 + 获取 IP）
wpeinit

REM 2) 映射网络安装源（改成你的 SMB 共享；建议用机器账号/只读共享）
net use Z: \\192.168.1.10\install /user:<username> <password>

REM 3) （可选）等待共享就绪
timeout /t 3 /nobreak >nul

REM 4) 启动 Windows 安装程序（图形界面，可再配合 autounattend.xml 全自动）
if exist Z:\setup.exe (
    echo Launching Windows setup from Z:\
    Z:\setup.exe
) else (
    echo ERROR: setup.exe not found on Z:\
    cmd.exe /k
)

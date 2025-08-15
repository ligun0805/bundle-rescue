go build -v -ldflags="-H=windowsgui" -o dist\bundlegui.exe .\cmd\bundlegui

go build -v -o dist/bundlecli.exe .\cmd\bundlecli
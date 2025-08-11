# Bundle Rescue — optimized build

## Сборка
```bash
go mod tidy

# GUI
go build -o bundlegui.exe ./cmd/bundlegui

# CLI
go build -o bundlecli.exe ./cmd/bundlecli
```

## Сборка **с иконкой**
### Вариант A — через Fyne (проще)
Установите CLI:
```bash
go install fyne.io/fyne/v2/cmd/fyne@latest
```
Убедитесь, что в корне есть `fyne.yaml` и `assets/icon.png`, затем:
```bash
# Windows .exe с иконкой
fyne package -os windows -icon assets/icon.png -name bundlegui -appID com.example.bundlerescue -release -executable bundlegui.exe
```
Файл `bundlegui.exe` в пакете будет с указанной иконкой.

### Вариант B — через goversioninfo (Windows)
```bash
go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest
```
Создайте `versioninfo.json`:
```json
{
  "FixedFileInfo": { "FileVersion": { "Major":0, "Minor":1, "Patch":0, "Build":0 } },
  "StringFileInfo": { "ProductName":"Bundle Rescue", "FileDescription":"Rescue ERC20 with Flashbots" },
  "IconPath": "assets/icon.png"
}
```
Сгенерируйте ресурс:
```bash
goversioninfo -icon=assets/icon.png
```
Появится `resource.syso`. Теперь обычный билд автоматически подхватит иконку:
```bash
go build -o bundlegui.exe ./cmd/bundlegui
```

## Примечания по UI
- Кастомная мягкая тёмная тема, градиентный фон.
- Левый столбец и Output — прокручиваемые.
- Моноширинная таблица без разрывов слов.
- Масштабируемые отступы/шрифты.

## Логирование
- Ядро (`internal/bundlecore/core.go`) пишет: head/baseFee, gas estimate, fee caps/tips, estPlus, список релеев/результаты предсимуляции, целевые блоки.
- GUI пишет логи действий и сырой ответ `eth_callBundle` во вкладке Sim Details; экспорт телеметрии в JSON.

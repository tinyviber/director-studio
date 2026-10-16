package main

import (
	"log"
	"os"

	"github.com/egoist/mygo"
)

// Director Studio：MyGo 导演工作台入口。
// 前端（web/）经 mygo generate 生成的 mygo.ts 调用 Studio 的绑定方法。
// 子命令：`studio genworkbench <dir>` 生成示例 workspace（不起 UI）。
func main() {
	if len(os.Args) > 2 && os.Args[1] == "genworkbench" {
		if err := genWorkbench(os.Args[2]); err != nil {
			log.Fatal(err)
		}
		return
	}
	mygo.Bind(&Studio{})

	mygo.App.WhenReady(func() {
		mygo.NewWindow(mygo.WindowOptions{
			Title:           "导演工作台",
			Width:           1440,
			Height:          900,
			MinWidth:        1100,
			MinHeight:       700,
			BackgroundColor: "#f4f4f6",
			StateKey:        "studio",
			URL:             "/",
		})
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

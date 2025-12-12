package main

import (
	"html/template"
	"log"
)

func main() {
	log.Println("开始测试模板加载...")
	
	// 测试加载主模板
	templates, err := template.ParseGlob("./web/templates/*.html")
	if err != nil {
		log.Fatalf("加载主模板失败: %v", err)
	}
	log.Println("主模板加载成功")
	
	// 测试加载partials模板
	templates, err = templates.ParseGlob("./web/templates/partials/*.html")
	if err != nil {
		log.Fatalf("加载partials模板失败: %v", err)
	}
	log.Println("Partials模板加载成功")
	
	// 列出所有已加载的模板
	log.Println("已加载的模板:")
	for _, t := range templates.Templates() {
		log.Printf("- %s", t.Name())
	}
	
	log.Println("模板测试完成！")
}
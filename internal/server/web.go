package server

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed web
var webFiles embed.FS

func (s *Server) registerWeb() {
	root, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	assets, err := fs.Sub(root, "assets")
	if err != nil {
		panic(err)
	}
	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		panic(err)
	}

	s.r.GET("/", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Data(http.StatusOK, "text/html; charset=utf-8", index)
	})
	s.r.GET("/assets/*filepath", gin.WrapH(
		http.StripPrefix("/assets/", http.FileServer(http.FS(assets))),
	))
}

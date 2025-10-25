package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/template/html/v2"
)

func main() {
	engine := html.New("../../internal/view", ".html")
	app := fiber.New(fiber.Config{
		Views: engine,
	})

	app.Use(logger.New())

	// Serve static files from the public directory (relative to project root)
	app.Static("/public", "../../public")

	// Home page
	app.Get("/", func(c *fiber.Ctx) error {
		// Render index within layouts/main
		return c.Render("index", fiber.Map{
			"Title": "Grimoire - Home",
		})
	})

	app.Get("/submit/:id", func(c *fiber.Ctx) error {
		jobID := c.Params("id")

		// Fetch job status from API server
		resp, err := http.Get("http://localhost:8081/api/" + jobID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "Failed to fetch job status",
			})
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "Failed to read job status",
			})
		}

		var jobData map[string]any
		if err := json.Unmarshal(body, &jobData); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "Failed to parse job status",
			})
		}

		// Extract status from the API response
		status, ok := jobData["status"].(string)
		if !ok {
			status = "unknown"
		}

		return c.Render("partial/job", fiber.Map{
			"ID":     jobID,
			"Status": status,
		})
	})

	log.Println("Web Server listening on http://localhost:8080")
	log.Fatal(app.Listen(":8080"))
}

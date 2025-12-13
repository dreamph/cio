package main

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
)

type User struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

var users = []User{
	{ID: 1, Name: "John"},
	{ID: 2, Name: "Jane"},
}

func main() {
	app := fiber.New()
	app.Use(logger.New())

	// GET /users
	app.Get("/users", func(c *fiber.Ctx) error {
		return c.JSON(users)
	})

	// GET /users/:id
	app.Get("/users/:id", func(c *fiber.Ctx) error {
		id, _ := c.ParamsInt("id")
		for _, u := range users {
			if u.ID == id {
				return c.JSON(u)
			}
		}
		return c.Status(404).JSON(fiber.Map{"error": "not found"})
	})

	// POST /users
	app.Post("/users", func(c *fiber.Ctx) error {
		var u User
		if err := c.BodyParser(&u); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": err.Error()})
		}
		u.ID = len(users) + 1
		users = append(users, u)
		return c.Status(201).JSON(u)
	})

	// PUT /users/:id
	app.Put("/users/:id", func(c *fiber.Ctx) error {
		id, _ := c.ParamsInt("id")
		var u User
		if err := c.BodyParser(&u); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": err.Error()})
		}
		for i, user := range users {
			if user.ID == id {
				u.ID = id
				users[i] = u
				return c.JSON(u)
			}
		}
		return c.Status(404).JSON(fiber.Map{"error": "not found"})
	})

	// DELETE /users/:id
	app.Delete("/users/:id", func(c *fiber.Ctx) error {
		id, _ := c.ParamsInt("id")
		for i, u := range users {
			if u.ID == id {
				users = append(users[:i], users[i+1:]...)
				return c.SendStatus(204)
			}
		}
		return c.Status(404).JSON(fiber.Map{"error": "not found"})
	})

	// Echo endpoint - returns what you send
	app.Post("/echo", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"headers": c.GetReqHeaders(),
			"body":    string(c.Body()),
		})
	})

	// Delay endpoint - for timeout testing
	app.Get("/delay/:ms", func(c *fiber.Ctx) error {
		ms, _ := c.ParamsInt("ms")
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return c.JSON(fiber.Map{"delayed": ms})
	})

	app.Listen(":3000")
}

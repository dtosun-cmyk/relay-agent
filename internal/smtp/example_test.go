package smtp_test

import (
	"bytes"
	"fmt"
	"log"

	"github.com/rs/zerolog"
	"relay-agent/internal/smtp"
)

// This example demonstrates how to create a new SMTP user manager
func ExampleNewUserManager() {
	logger := zerolog.New(zerolog.NewConsoleWriter()).With().Timestamp().Logger()
	domain := "smtp.example.com"

	_ = smtp.NewUserManager(domain, logger)

	fmt.Printf("UserManager created for domain: %s\n", domain)

	// Output:
	// UserManager created for domain: smtp.example.com
}

// This example demonstrates the complete workflow of managing SMTP users
func ExampleUserManager_workflow() {
	// Setup logger (in production, use proper file logging)
	var buf bytes.Buffer
	logger := zerolog.New(&buf).With().Timestamp().Logger()

	// Create user manager with your SMTP domain
	_ = smtp.NewUserManager("smtp.example.com", logger)

	// Note: The following operations require saslpasswd2 to be installed
	// and proper permissions. In a real environment:

	// Create a user
	// err := manager.CreateUser("john_doe", "SecurePassword123")
	// if err != nil {
	//     log.Fatalf("Failed to create user: %v", err)
	// }

	// List all users
	// users, err := manager.ListUsers()
	// if err != nil {
	//     log.Fatalf("Failed to list users: %v", err)
	// }
	// for _, user := range users {
	//     fmt.Printf("User: %s@%s\n", user.Username, user.Domain)
	// }

	// Check if user exists
	// exists, err := manager.UserExists("john_doe")
	// if err != nil {
	//     log.Fatalf("Failed to check user: %v", err)
	// }
	// if exists {
	//     fmt.Println("User exists!")
	// }

	// Update password
	// err = manager.UpdatePassword("john_doe", "NewSecurePassword456")
	// if err != nil {
	//     log.Fatalf("Failed to update password: %v", err)
	// }

	// Delete user
	// err = manager.DeleteUser("john_doe")
	// if err != nil {
	//     log.Fatalf("Failed to delete user: %v", err)
	// }

	fmt.Println("SMTP user management workflow example completed")

	// Output:
	// SMTP user management workflow example completed
}

// This example shows how to handle errors when creating users
func ExampleUserManager_CreateUser_errors() {
	var buf bytes.Buffer
	logger := zerolog.New(&buf).With().Timestamp().Logger()
	manager := smtp.NewUserManager("smtp.example.com", logger)

	// These will fail validation (without executing saslpasswd2)

	// Invalid username - contains special characters
	err := manager.CreateUser("user@invalid", "password123")
	if err == smtp.ErrInvalidUsername {
		fmt.Println("Username validation failed as expected")
	}

	// Invalid password - too short
	err = manager.CreateUser("validuser", "short")
	if err == smtp.ErrInvalidPassword {
		fmt.Println("Password validation failed as expected")
	}

	// Output:
	// Username validation failed as expected
	// Password validation failed as expected
}

// This example demonstrates the CreateUserRequest structure used in API calls
func ExampleCreateUserRequest() {
	// This is the JSON structure expected by the API
	request := smtp.CreateUserRequest{
		Username: "newuser",
		Password: "SecurePassword123",
	}

	fmt.Printf("Username: %s\n", request.Username)
	fmt.Printf("Password length: %d\n", len(request.Password))

	// Output:
	// Username: newuser
	// Password length: 17
}

// This example shows the User structure returned by ListUsers
func ExampleUser() {
	user := smtp.User{
		Username:  "john_doe",
		Domain:    "smtp.example.com",
		CreatedAt: "2023-01-01T12:00:00Z",
	}

	fmt.Printf("User: %s@%s (created: %s)\n", user.Username, user.Domain, user.CreatedAt)

	// Output:
	// User: john_doe@smtp.example.com (created: 2023-01-01T12:00:00Z)
}

// This example demonstrates proper error handling in a real application
func ExampleUserManager_errorHandling() {
	var buf bytes.Buffer
	logger := zerolog.New(&buf).With().Timestamp().Logger()
	manager := smtp.NewUserManager("smtp.example.com", logger)

	username := "testuser"
	password := "TestPassword123"

	// Attempt to create a user with proper error handling
	err := manager.CreateUser(username, password)
	switch err {
	case nil:
		log.Println("User created successfully")
	case smtp.ErrInvalidUsername:
		log.Println("Invalid username format")
	case smtp.ErrInvalidPassword:
		log.Println("Invalid password format")
	case smtp.ErrUserExists:
		log.Println("User already exists")
	default:
		log.Printf("Failed to create user: %v", err)
	}

	fmt.Println("Error handling example completed")

	// Output:
	// Error handling example completed
}

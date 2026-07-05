// ABOUTME: Meal Planning extension — recipes, weekly meal plans, and shopping lists.
// ABOUTME: Adds tools for managing recipes, planning meals, and generating grocery lists.

package meals

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"substrate/brain"
)

// Register adds meal planning tools to the MCP server.
func Register(s *server.MCPServer, a *brain.App) {
	s.AddTool(mcp.NewTool("add_recipe",
		mcp.WithDescription("Add a recipe with ingredients and instructions"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Recipe name")),
		mcp.WithString("cuisine", mcp.Description("Cuisine type (e.g. 'Italian', 'Asian', 'Mexican')")),
		mcp.WithNumber("prep_time_minutes", mcp.Description("Prep time in minutes")),
		mcp.WithNumber("cook_time_minutes", mcp.Description("Cook time in minutes")),
		mcp.WithNumber("servings", mcp.Description("Number of servings")),
		mcp.WithString("ingredients", mcp.Description("JSON array of ingredients, e.g. '[{\"name\":\"chicken\",\"quantity\":\"1\",\"unit\":\"lb\"}]'")),
		mcp.WithString("instructions", mcp.Description("JSON array of instruction steps, e.g. '[\"Preheat oven\",\"Mix ingredients\"]'")),
		mcp.WithString("tags", mcp.Description("Comma-separated tags (e.g. 'quick, healthy, asian')")),
		mcp.WithString("notes", mcp.Description("Additional notes")),
	), addRecipe(a))

	s.AddTool(mcp.NewTool("search_recipes",
		mcp.WithDescription("Search recipes by name, cuisine, tag, or ingredient"),
		mcp.WithString("query", mcp.Description("Search term (matches name and notes)")),
		mcp.WithString("cuisine", mcp.Description("Filter by cuisine type")),
		mcp.WithString("tag", mcp.Description("Filter by tag")),
		mcp.WithString("ingredient", mcp.Description("Filter by ingredient name")),
	), searchRecipes(a))

	s.AddTool(mcp.NewTool("update_recipe",
		mcp.WithDescription("Update an existing recipe's fields"),
		mcp.WithString("recipe_id", mcp.Required(), mcp.Description("Recipe ID to update")),
		mcp.WithString("name", mcp.Description("New name")),
		mcp.WithString("cuisine", mcp.Description("New cuisine")),
		mcp.WithNumber("prep_time_minutes", mcp.Description("New prep time")),
		mcp.WithNumber("cook_time_minutes", mcp.Description("New cook time")),
		mcp.WithNumber("servings", mcp.Description("New servings")),
		mcp.WithString("ingredients", mcp.Description("New ingredients JSON array")),
		mcp.WithString("instructions", mcp.Description("New instructions JSON array")),
		mcp.WithString("tags", mcp.Description("New comma-separated tags")),
		mcp.WithString("notes", mcp.Description("New notes")),
	), updateRecipe(a))

	s.AddTool(mcp.NewTool("create_meal_plan",
		mcp.WithDescription("Plan a meal for a specific day and meal type"),
		mcp.WithString("week_start", mcp.Required(), mcp.Description("Monday of the week (YYYY-MM-DD)")),
		mcp.WithString("day_of_week", mcp.Required(), mcp.Description("Day of week (monday–sunday)")),
		mcp.WithString("meal_type", mcp.Required(), mcp.Description("Meal type (breakfast, lunch, dinner, snack)")),
		mcp.WithString("recipe_id", mcp.Description("Recipe ID (omit for custom meal)")),
		mcp.WithString("custom_meal", mcp.Description("Custom meal name if no recipe")),
		mcp.WithNumber("servings", mcp.Description("Number of servings (default 4)")),
		mcp.WithString("notes", mcp.Description("Additional notes")),
	), createMealPlan(a))

	s.AddTool(mcp.NewTool("get_meal_plan",
		mcp.WithDescription("View the meal plan for a given week"),
		mcp.WithString("week_start", mcp.Required(), mcp.Description("Monday of the week (YYYY-MM-DD)")),
	), getMealPlan(a))

	s.AddTool(mcp.NewTool("generate_shopping_list",
		mcp.WithDescription("Auto-generate a shopping list from a week's meal plan"),
		mcp.WithString("week_start", mcp.Required(), mcp.Description("Monday of the week (YYYY-MM-DD)")),
	), generateShoppingList(a))
}

type recipe struct {
	ID           string
	Name         string
	Cuisine      *string
	PrepTime     *int
	CookTime     *int
	Servings     *int
	Ingredients  json.RawMessage
	Instructions json.RawMessage
	Tags         []string
	Notes        *string
	CreatedAt    time.Time
}

type mealPlanEntry struct {
	DayOfWeek  string
	MealType   string
	RecipeName *string
	CustomMeal *string
	Servings   *int
	Notes      *string
}

type ingredient struct {
	Name     string `json:"name"`
	Quantity string `json:"quantity"`
	Unit     string `json:"unit"`
}

func addRecipe(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, _ := req.GetArguments()["name"].(string)
		if name == "" {
			return brain.ToolError("name is required"), nil
		}
		cuisine, _ := req.GetArguments()["cuisine"].(string)
		ingredientsStr, _ := req.GetArguments()["ingredients"].(string)
		instructionsStr, _ := req.GetArguments()["instructions"].(string)
		tagsStr, _ := req.GetArguments()["tags"].(string)
		notes, _ := req.GetArguments()["notes"].(string)

		var prepTime, cookTime, servings *int
		if v, ok := req.GetArguments()["prep_time_minutes"].(float64); ok {
			i := int(v)
			prepTime = &i
		}
		if v, ok := req.GetArguments()["cook_time_minutes"].(float64); ok {
			i := int(v)
			cookTime = &i
		}
		if v, ok := req.GetArguments()["servings"].(float64); ok {
			i := int(v)
			servings = &i
		}

		ingredients := json.RawMessage("[]")
		if ingredientsStr != "" {
			ingredients = json.RawMessage(ingredientsStr)
		}
		instructions := json.RawMessage("[]")
		if instructionsStr != "" {
			instructions = json.RawMessage(instructionsStr)
		}

		var tags []string
		if tagsStr != "" {
			for _, t := range strings.Split(tagsStr, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					tags = append(tags, t)
				}
			}
		}

		var id string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO recipes (user_id, name, cuisine, prep_time_minutes, cook_time_minutes, servings, ingredients, instructions, tags, notes)
				VALUES (current_setting('app.current_user_id')::uuid, $1, $2, $3, $4, $5, $6, $7, $8, $9)
				RETURNING id::text`,
				name, nullOrStr(cuisine), prepTime, cookTime, servings,
				ingredients, instructions, tags, nullOrStr(notes),
			).Scan(&id)
		})
		if err != nil {
			return brain.ToolError("Failed to add recipe: " + err.Error()), nil
		}

		parts := []string{fmt.Sprintf("Added recipe: %s (id: %s)", name, id)}
		if cuisine != "" {
			parts = append(parts, "Cuisine: "+cuisine)
		}
		if len(tags) > 0 {
			parts = append(parts, "Tags: "+strings.Join(tags, ", "))
		}
		return brain.TextResult(strings.Join(parts, "\n")), nil
	}
}

func searchRecipes(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, _ := req.GetArguments()["query"].(string)
		cuisine, _ := req.GetArguments()["cuisine"].(string)
		tag, _ := req.GetArguments()["tag"].(string)
		ingredientName, _ := req.GetArguments()["ingredient"].(string)

		var recipes []recipe
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			sql := `SELECT id::text, name, cuisine, prep_time_minutes, cook_time_minutes, servings,
			               ingredients, instructions, tags, notes, created_at
			        FROM recipes WHERE true`
			args := []any{}
			n := 1

			if query != "" {
				sql += fmt.Sprintf(" AND (name ILIKE $%d OR notes ILIKE $%d)", n, n)
				args = append(args, "%"+query+"%")
				n++
			}
			if cuisine != "" {
				sql += fmt.Sprintf(" AND cuisine ILIKE $%d", n)
				args = append(args, "%"+cuisine+"%")
				n++
			}
			if tag != "" {
				sql += fmt.Sprintf(" AND $%d = ANY(tags)", n)
				args = append(args, tag)
				n++
			}
			if ingredientName != "" {
				sql += fmt.Sprintf(" AND ingredients::text ILIKE $%d", n)
				args = append(args, "%"+ingredientName+"%")
				n++
			}
			sql += " ORDER BY name ASC"

			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var r recipe
				if err := rows.Scan(&r.ID, &r.Name, &r.Cuisine, &r.PrepTime, &r.CookTime,
					&r.Servings, &r.Ingredients, &r.Instructions, &r.Tags, &r.Notes, &r.CreatedAt); err != nil {
					return err
				}
				recipes = append(recipes, r)
			}
			return rows.Err()
		})
		if err != nil {
			return brain.ToolError("Error: " + err.Error()), nil
		}

		if len(recipes) == 0 {
			return brain.TextResult("No recipes found."), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "%d recipe(s):\n\n", len(recipes))
		for _, r := range recipes {
			fmt.Fprintf(&sb, "• %s (id: %s)\n", r.Name, r.ID)
			if r.Cuisine != nil {
				fmt.Fprintf(&sb, "  Cuisine: %s\n", *r.Cuisine)
			}
			if r.PrepTime != nil || r.CookTime != nil {
				parts := []string{}
				if r.PrepTime != nil {
					parts = append(parts, fmt.Sprintf("prep %dm", *r.PrepTime))
				}
				if r.CookTime != nil {
					parts = append(parts, fmt.Sprintf("cook %dm", *r.CookTime))
				}
				fmt.Fprintf(&sb, "  Time: %s\n", strings.Join(parts, ", "))
			}
			if r.Servings != nil {
				fmt.Fprintf(&sb, "  Serves: %d\n", *r.Servings)
			}
			if len(r.Tags) > 0 {
				fmt.Fprintf(&sb, "  Tags: %s\n", strings.Join(r.Tags, ", "))
			}
			fmt.Fprintln(&sb)
		}
		return brain.TextResult(sb.String()), nil
	}
}

func updateRecipe(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		recipeID, _ := req.GetArguments()["recipe_id"].(string)
		if recipeID == "" {
			return brain.ToolError("recipe_id is required"), nil
		}

		sets := []string{}
		args := []any{}
		n := 1

		if v, ok := req.GetArguments()["name"].(string); ok && v != "" {
			sets = append(sets, fmt.Sprintf("name = $%d", n))
			args = append(args, v)
			n++
		}
		if v, ok := req.GetArguments()["cuisine"].(string); ok && v != "" {
			sets = append(sets, fmt.Sprintf("cuisine = $%d", n))
			args = append(args, v)
			n++
		}
		if v, ok := req.GetArguments()["prep_time_minutes"].(float64); ok {
			sets = append(sets, fmt.Sprintf("prep_time_minutes = $%d", n))
			args = append(args, int(v))
			n++
		}
		if v, ok := req.GetArguments()["cook_time_minutes"].(float64); ok {
			sets = append(sets, fmt.Sprintf("cook_time_minutes = $%d", n))
			args = append(args, int(v))
			n++
		}
		if v, ok := req.GetArguments()["servings"].(float64); ok {
			sets = append(sets, fmt.Sprintf("servings = $%d", n))
			args = append(args, int(v))
			n++
		}
		if v, ok := req.GetArguments()["ingredients"].(string); ok && v != "" {
			sets = append(sets, fmt.Sprintf("ingredients = $%d", n))
			args = append(args, json.RawMessage(v))
			n++
		}
		if v, ok := req.GetArguments()["instructions"].(string); ok && v != "" {
			sets = append(sets, fmt.Sprintf("instructions = $%d", n))
			args = append(args, json.RawMessage(v))
			n++
		}
		if v, ok := req.GetArguments()["tags"].(string); ok && v != "" {
			var tags []string
			for _, t := range strings.Split(v, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					tags = append(tags, t)
				}
			}
			sets = append(sets, fmt.Sprintf("tags = $%d", n))
			args = append(args, tags)
			n++
		}
		if v, ok := req.GetArguments()["notes"].(string); ok && v != "" {
			sets = append(sets, fmt.Sprintf("notes = $%d", n))
			args = append(args, v)
			n++
		}

		if len(sets) == 0 {
			return brain.ToolError("No fields to update"), nil
		}

		args = append(args, recipeID)
		sql := fmt.Sprintf("UPDATE recipes SET %s WHERE id = $%d RETURNING name", strings.Join(sets, ", "), n)

		var updatedName string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, sql, args...).Scan(&updatedName)
		})
		if err != nil {
			return brain.ToolError("Failed to update recipe: " + err.Error()), nil
		}

		return brain.TextResult(fmt.Sprintf("Updated recipe: %s", updatedName)), nil
	}
}

func createMealPlan(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		weekStart, _ := req.GetArguments()["week_start"].(string)
		dayOfWeek, _ := req.GetArguments()["day_of_week"].(string)
		mealType, _ := req.GetArguments()["meal_type"].(string)
		if weekStart == "" || dayOfWeek == "" || mealType == "" {
			return brain.ToolError("week_start, day_of_week, and meal_type are required"), nil
		}

		recipeID, _ := req.GetArguments()["recipe_id"].(string)
		customMeal, _ := req.GetArguments()["custom_meal"].(string)
		notes, _ := req.GetArguments()["notes"].(string)

		servings := 4
		if v, ok := req.GetArguments()["servings"].(float64); ok && v > 0 {
			servings = int(v)
		}

		var id string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO meal_plans (user_id, week_start, day_of_week, meal_type, recipe_id, custom_meal, servings, notes)
				VALUES (current_setting('app.current_user_id')::uuid, $1::date, $2, $3, NULLIF($4, '')::uuid, $5, $6, $7)
				ON CONFLICT (user_id, week_start, day_of_week, meal_type)
				DO UPDATE SET recipe_id = EXCLUDED.recipe_id, custom_meal = EXCLUDED.custom_meal,
				             servings = EXCLUDED.servings, notes = EXCLUDED.notes
				RETURNING id::text`,
				weekStart, strings.ToLower(dayOfWeek), strings.ToLower(mealType),
				recipeID, nullOrStr(customMeal), servings, nullOrStr(notes),
			).Scan(&id)
		})
		if err != nil {
			return brain.ToolError("Failed to create meal plan: " + err.Error()), nil
		}

		meal := customMeal
		if recipeID != "" {
			meal = fmt.Sprintf("recipe %s", recipeID)
		}
		return brain.TextResult(fmt.Sprintf("Planned %s %s for %s: %s (serves %d)", dayOfWeek, mealType, weekStart, meal, servings)), nil
	}
}

func getMealPlan(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		weekStart, _ := req.GetArguments()["week_start"].(string)
		if weekStart == "" {
			return brain.ToolError("week_start is required"), nil
		}

		var entries []mealPlanEntry
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `
				SELECT mp.day_of_week, mp.meal_type, r.name, mp.custom_meal, mp.servings, mp.notes
				FROM meal_plans mp
				LEFT JOIN recipes r ON r.id = mp.recipe_id
				WHERE mp.week_start = $1::date
				ORDER BY
				    CASE mp.day_of_week
				        WHEN 'monday' THEN 1 WHEN 'tuesday' THEN 2 WHEN 'wednesday' THEN 3
				        WHEN 'thursday' THEN 4 WHEN 'friday' THEN 5 WHEN 'saturday' THEN 6
				        WHEN 'sunday' THEN 7 END,
				    CASE mp.meal_type
				        WHEN 'breakfast' THEN 1 WHEN 'lunch' THEN 2
				        WHEN 'dinner' THEN 3 WHEN 'snack' THEN 4 END`,
				weekStart)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var e mealPlanEntry
				if err := rows.Scan(&e.DayOfWeek, &e.MealType, &e.RecipeName, &e.CustomMeal, &e.Servings, &e.Notes); err != nil {
					return err
				}
				entries = append(entries, e)
			}
			return rows.Err()
		})
		if err != nil {
			return brain.ToolError("Error: " + err.Error()), nil
		}

		if len(entries) == 0 {
			return brain.TextResult("No meal plan for the week of " + weekStart), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "Meal plan for week of %s:\n", weekStart)
		currentDay := ""
		for _, e := range entries {
			if e.DayOfWeek != currentDay {
				currentDay = e.DayOfWeek
				fmt.Fprintf(&sb, "\n%s:\n", titleCase(currentDay))
			}
			meal := "TBD"
			if e.RecipeName != nil {
				meal = *e.RecipeName
			} else if e.CustomMeal != nil {
				meal = *e.CustomMeal
			}
			fmt.Fprintf(&sb, "  %s: %s", titleCase(e.MealType), meal)
			if e.Servings != nil {
				fmt.Fprintf(&sb, " (serves %d)", *e.Servings)
			}
			fmt.Fprintln(&sb)
		}
		return brain.TextResult(sb.String()), nil
	}
}

func generateShoppingList(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		weekStart, _ := req.GetArguments()["week_start"].(string)
		if weekStart == "" {
			return brain.ToolError("week_start is required"), nil
		}

		type listItem struct {
			Name         string
			Quantity     string
			Unit         string
			RecipeSource string
		}

		var items []listItem
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			// Clear existing list for this week.
			_, err := tx.Exec(ctx,
				"DELETE FROM shopping_lists WHERE week_start = $1::date",
				weekStart)
			if err != nil {
				return err
			}

			// Get all recipes for this week's meal plan.
			rows, err := tx.Query(ctx, `
				SELECT r.name, r.ingredients
				FROM meal_plans mp
				JOIN recipes r ON r.id = mp.recipe_id
				WHERE mp.week_start = $1::date`, weekStart)
			if err != nil {
				return err
			}
			defer rows.Close()

			for rows.Next() {
				var recipeName string
				var ingredientsJSON json.RawMessage
				if err := rows.Scan(&recipeName, &ingredientsJSON); err != nil {
					return err
				}

				var ings []ingredient
				if err := json.Unmarshal(ingredientsJSON, &ings); err != nil {
					continue
				}

				for _, ing := range ings {
					items = append(items, listItem{
						Name:         ing.Name,
						Quantity:     ing.Quantity,
						Unit:         ing.Unit,
						RecipeSource: recipeName,
					})
				}
			}
			if err := rows.Err(); err != nil {
				return err
			}

			// Insert all items into shopping_lists.
			for _, it := range items {
				_, err := tx.Exec(ctx, `
					INSERT INTO shopping_lists (user_id, week_start, item_name, quantity, unit, recipe_source)
					VALUES (current_setting('app.current_user_id')::uuid, $1::date, $2, $3, $4, $5)`,
					weekStart, it.Name, nullOrStr(it.Quantity), nullOrStr(it.Unit), it.RecipeSource)
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return brain.ToolError("Error generating shopping list: " + err.Error()), nil
		}

		if len(items) == 0 {
			return brain.TextResult("No recipe-based meals found for the week of " + weekStart + ". Shopping list is empty."), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "Shopping list for week of %s (%d items):\n\n", weekStart, len(items))
		for _, it := range items {
			qty := ""
			if it.Quantity != "" {
				qty = it.Quantity
				if it.Unit != "" {
					qty += " " + it.Unit
				}
				qty += " "
			}
			fmt.Fprintf(&sb, "• %s%s (from: %s)\n", qty, it.Name, it.RecipeSource)
		}
		return brain.TextResult(sb.String()), nil
	}
}

func nullOrStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

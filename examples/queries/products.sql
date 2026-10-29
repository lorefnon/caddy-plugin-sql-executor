-- List products, optionally filtered by category.
-- POST /api/products            {"category": "coffee"}
-- POST /api/products.csv        {"category": null}
SELECT id, name, category, price, in_stock, added_on
FROM products
WHERE (:category IS NULL OR category = :category)
ORDER BY id

INSERT INTO products (name, category, price, added_on)
VALUES (:name, :category, :price, date('now'))
RETURNING id, name, category, price

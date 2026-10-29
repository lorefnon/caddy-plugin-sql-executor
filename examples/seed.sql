CREATE TABLE IF NOT EXISTS products (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    category   TEXT NOT NULL,
    price      REAL NOT NULL,
    in_stock   BOOLEAN NOT NULL DEFAULT 1,
    added_on   DATE NOT NULL,
    added_by   INTEGER
);

INSERT INTO products (name, category, price, in_stock, added_on, added_by) VALUES
    ('Espresso beans', 'coffee', 14.50, 1, '2024-01-15', 1),
    ('Pour-over kettle', 'equipment', 39.00, 1, '2024-02-01', 2),
    ('Ceramic dripper', 'equipment', 22.00, 0, '2024-03-10', 1),
    ('Decaf blend', 'coffee', 12.75, 1, '2024-04-22', 2);

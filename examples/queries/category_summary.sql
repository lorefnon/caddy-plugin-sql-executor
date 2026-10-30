-- Product counts and prices per category.
-- @column avg_price = {description: "Average price, rounded to cents."}
SELECT category,
       count(*)              AS products,
       round(avg(price), 2)  AS avg_price,
       sum(in_stock)         AS in_stock
FROM products
GROUP BY category
ORDER BY category

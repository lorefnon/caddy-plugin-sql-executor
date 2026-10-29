-- Products added by the calling user. X-User-Id would normally be set by an
-- auth layer in front of this route (e.g. forward_auth ... copy_headers).
-- POST /api/products/mine   with header  X-User-Id: 1
-- @header :user_id = X-User-Id
SELECT id, name, category, price
FROM products
WHERE added_by = :user_id
ORDER BY id

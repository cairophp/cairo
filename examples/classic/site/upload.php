<?php

// An upload as PHP gives it to a script: $_FILES, and move_uploaded_file()
// for a file that came with this request only.

declare(strict_types=1);

$file = $_FILES['doc'] ?? null;
$to = sys_get_temp_dir() . '/cairo-upload-' . bin2hex(random_bytes(4));
$moved = $file !== null && move_uploaded_file($file['tmp_name'], $to);
echo json_encode([
    'name' => $file['name'] ?? null,
    'size' => $file['size'] ?? null,
    'field' => $_POST['note'] ?? null,
    'content' => $moved ? file_get_contents($to) : null,
    'foreign' => is_uploaded_file(__FILE__),
]), "\n";
if ($moved) {
    unlink($to);
}

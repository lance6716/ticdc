drop database if exists `lightning_mode`;
create database `lightning_mode`;
use `lightning_mode`;
create table t2 (
    id int NOT NULL AUTO_INCREMENT,
    name varchar(20),
    PRIMARY KEY (id));;
insert into t2 (name) values ('Arya'), ('Bran'), ('Sansa');

-- test duplicate detection
create table dup (
    id INT PRIMARY KEY,
    name VARCHAR(20)
);

insert into dup values (1, 'a2'), (2, 'b2'), (3, 'c2');

-- test block-allow-list
drop database if exists `ignore_db`;
create database `ignore_db`;
use `ignore_db`;
create table `ignore_table`(id int);
